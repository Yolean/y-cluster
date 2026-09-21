package hetzner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
)

// fakeCloud is an in-process Hetzner Cloud API that keeps state, so
// tests assert on what a project holds after Provision or Teardown
// rather than on a sequence of calls. It implements the endpoints
// this package uses and two rules of the real API that the teardown
// order exists for: names are unique per resource kind, and a
// certificate an LB service references cannot be deleted.
type fakeCloud struct {
	t  *testing.T
	mu sync.Mutex

	nextID  int64
	servers map[int64]*schema.Server
	keys    map[int64]*schema.SSHKey
	certs   map[int64]*schema.Certificate
	lbs     map[int64]*schema.LoadBalancer

	// failures maps "METHOD /path-prefix" to a message. A matching
	// request is refused with 422, which hcloud-go does not retry.
	failures map[string]string
	// failedActions: a request matching the key succeeds, and the
	// action it returns has failed.
	failedActions map[string]string
	// requests is every "METHOD /path" seen, in order.
	requests []string
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	f := &fakeCloud{
		t:             t,
		nextID:        100,
		servers:       map[int64]*schema.Server{},
		keys:          map[int64]*schema.SSHKey{},
		certs:         map[int64]*schema.Certificate{},
		lbs:           map[int64]*schema.LoadBalancer{},
		failures:      map[string]string{},
		failedActions: map[string]string{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	prev := hcloudClientOptions
	hcloudClientOptions = []hcloud.ClientOption{
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond), MaxRetries: 1}),
	}
	t.Cleanup(func() { hcloudClientOptions = prev })
	t.Setenv(HCloudTokenEnv, "fake-token")
	return f
}

// inventory is what the project holds, as sorted "kind/name" entries.
func (f *fakeCloud) inventory() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.servers {
		out = append(out, "server/"+s.Name)
	}
	for _, k := range f.keys {
		out = append(out, "ssh_key/"+k.Name)
	}
	for _, c := range f.certs {
		out = append(out, "certificate/"+c.Name)
	}
	for _, lb := range f.lbs {
		out = append(out, "load_balancer/"+lb.Name)
	}
	sort.Strings(out)
	return out
}

// lbCertificates lists the certificate ids on an LB's 443 service.
func (f *fakeCloud) lbCertificates(name string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, lb := range f.lbs {
		if lb.Name != name {
			continue
		}
		for _, svc := range lb.Services {
			if svc.ListenPort == 443 && svc.HTTP != nil {
				return append([]int64(nil), svc.HTTP.Certificates...)
			}
		}
	}
	return nil
}

func (f *fakeCloud) id() int64 {
	f.nextID++
	return f.nextID
}

func (f *fakeCloud) action(key, command string) schema.Action {
	a := schema.Action{ID: f.id(), Command: command, Status: "success", Progress: 100}
	if msg, ok := f.failedActions[key]; ok {
		a.Status = "error"
		a.Error = &schema.ActionError{Code: "action_failed", Message: msg}
	}
	return a
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/")
	key := r.Method + " /" + path
	f.requests = append(f.requests, key)
	for prefix, msg := range f.failures {
		if strings.HasPrefix(key, prefix) {
			f.refuse(w, http.StatusUnprocessableEntity, "invalid_input", msg)
			return
		}
	}

	parts := strings.Split(path, "/")
	var id int64
	if len(parts) > 1 {
		id, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	switch parts[0] {
	case "servers":
		f.serveServers(w, r, key, parts, id)
	case "ssh_keys":
		f.serveKeys(w, r, parts, id)
	case "certificates":
		f.serveCertificates(w, r, parts, id)
	case "load_balancers":
		f.serveLoadBalancers(w, r, key, parts, id)
	default:
		f.t.Errorf("fake cloud: unexpected request %s", key)
		f.refuse(w, http.StatusNotFound, "not_found", key)
	}
}

func (f *fakeCloud) refuse(w http.ResponseWriter, status int, code, message string) {
	f.reply(w, status, schema.ErrorResponse{Error: schema.Error{Code: code, Message: message}})
}

func (f *fakeCloud) reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		f.t.Errorf("fake cloud: encode: %v", err)
	}
}

func decode[T any](f *fakeCloud, r *http.Request) T {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		f.t.Errorf("fake cloud: decode %s %s: %v", r.Method, r.URL.Path, err)
	}
	return v
}

// matches implements the equality subset of label selectors, which is
// all this package writes: "k=v,k2=v2".
func matches(labels map[string]string, selector string) bool {
	if selector == "" {
		return true
	}
	for _, term := range strings.Split(selector, ",") {
		k, v, ok := strings.Cut(term, "=")
		if !ok || labels[k] != v {
			return false
		}
	}
	return true
}

var onePage = schema.Meta{Pagination: &schema.MetaPagination{Page: 1, PerPage: 50, LastPage: 1}}

func (f *fakeCloud) serveServers(w http.ResponseWriter, r *http.Request, key string, parts []string, id int64) {
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		list := []schema.Server{}
		for _, s := range f.servers {
			if name := r.URL.Query().Get("name"); name != "" && s.Name != name {
				continue
			}
			if !matches(s.Labels, r.URL.Query().Get("label_selector")) {
				continue
			}
			list = append(list, *s)
		}
		f.reply(w, http.StatusOK, map[string]any{"servers": list, "meta": onePage})
	case r.Method == http.MethodPost && len(parts) == 1:
		req := decode[schema.ServerCreateRequest](f, r)
		for _, s := range f.servers {
			if s.Name == req.Name {
				f.refuse(w, http.StatusConflict, "uniqueness_error", "server name is already used")
				return
			}
		}
		s := &schema.Server{ID: f.id(), Name: req.Name, Status: "running", Created: time.Now()}
		s.PublicNet.IPv4.IP = "127.0.0.1"
		if req.Labels != nil {
			s.Labels = *req.Labels
		}
		f.servers[s.ID] = s
		f.reply(w, http.StatusCreated, schema.ServerCreateResponse{
			Server:      *s,
			Action:      f.action(key, "create_server"),
			NextActions: []schema.Action{f.action(key, "start_server")},
		})
	case r.Method == http.MethodGet && len(parts) == 2:
		if s, ok := f.servers[id]; ok {
			f.reply(w, http.StatusOK, schema.ServerGetResponse{Server: *s})
			return
		}
		f.refuse(w, http.StatusNotFound, "not_found", "server not found")
	case r.Method == http.MethodDelete && len(parts) == 2:
		if _, ok := f.servers[id]; !ok {
			f.refuse(w, http.StatusNotFound, "not_found", "server not found")
			return
		}
		delete(f.servers, id)
		f.reply(w, http.StatusOK, schema.ServerDeleteResponse{Action: f.action(key, "delete_server")})
	default:
		f.t.Errorf("fake cloud: unexpected request %s", key)
		f.refuse(w, http.StatusNotFound, "not_found", key)
	}
}

func (f *fakeCloud) serveKeys(w http.ResponseWriter, r *http.Request, parts []string, id int64) {
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		list := []schema.SSHKey{}
		for _, k := range f.keys {
			if name := r.URL.Query().Get("name"); name == "" || k.Name == name {
				list = append(list, *k)
			}
		}
		f.reply(w, http.StatusOK, map[string]any{"ssh_keys": list, "meta": onePage})
	case r.Method == http.MethodPost:
		req := decode[schema.SSHKeyCreateRequest](f, r)
		for _, k := range f.keys {
			if k.Name == req.Name {
				f.refuse(w, http.StatusConflict, "uniqueness_error", "SSH key name is already used")
				return
			}
		}
		k := &schema.SSHKey{ID: f.id(), Name: req.Name, PublicKey: req.PublicKey, Created: time.Now()}
		f.keys[k.ID] = k
		f.reply(w, http.StatusCreated, schema.SSHKeyCreateResponse{SSHKey: *k})
	case r.Method == http.MethodDelete && len(parts) == 2:
		delete(f.keys, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.refuse(w, http.StatusNotFound, "not_found", r.URL.Path)
	}
}

func (f *fakeCloud) serveCertificates(w http.ResponseWriter, r *http.Request, parts []string, id int64) {
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		list := []schema.Certificate{}
		for _, c := range f.certs {
			if name := r.URL.Query().Get("name"); name == "" || c.Name == name {
				list = append(list, *c)
			}
		}
		f.reply(w, http.StatusOK, map[string]any{"certificates": list, "meta": onePage})
	case r.Method == http.MethodPost:
		req := decode[schema.CertificateCreateRequest](f, r)
		c := &schema.Certificate{ID: f.id(), Name: req.Name, Type: req.Type, Certificate: req.Certificate, Created: time.Now()}
		if req.Labels != nil {
			c.Labels = *req.Labels
		}
		f.certs[c.ID] = c
		f.reply(w, http.StatusCreated, schema.CertificateCreateResponse{Certificate: *c})
	case r.Method == http.MethodGet && len(parts) == 2:
		if c, ok := f.certs[id]; ok {
			f.reply(w, http.StatusOK, schema.CertificateGetResponse{Certificate: *c})
			return
		}
		f.refuse(w, http.StatusNotFound, "not_found", "certificate not found")
	case r.Method == http.MethodDelete && len(parts) == 2:
		for _, lb := range f.lbs {
			for _, svc := range lb.Services {
				if svc.HTTP == nil {
					continue
				}
				for _, certID := range svc.HTTP.Certificates {
					if certID == id {
						f.refuse(w, http.StatusConflict, "resource_in_use", fmt.Sprintf("certificate is used by load balancer %s", lb.Name))
						return
					}
				}
			}
		}
		delete(f.certs, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.refuse(w, http.StatusNotFound, "not_found", r.URL.Path)
	}
}

func (f *fakeCloud) serveLoadBalancers(w http.ResponseWriter, r *http.Request, key string, parts []string, id int64) {
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		list := []schema.LoadBalancer{}
		for _, lb := range f.lbs {
			if name := r.URL.Query().Get("name"); name == "" || lb.Name == name {
				list = append(list, *lb)
			}
		}
		f.reply(w, http.StatusOK, map[string]any{"load_balancers": list, "meta": onePage})
	case r.Method == http.MethodPost && len(parts) == 1:
		req := decode[schema.LoadBalancerCreateRequest](f, r)
		lb := &schema.LoadBalancer{ID: f.id(), Name: req.Name, Created: time.Now()}
		lb.PublicNet.Enabled = true
		lb.PublicNet.IPv4.IP = "192.0.2.10"
		if req.Location != nil {
			lb.Location.Name = *req.Location
		}
		if req.Labels != nil {
			lb.Labels = *req.Labels
		}
		for _, svc := range req.Services {
			out := schema.LoadBalancerService{Protocol: svc.Protocol}
			if svc.ListenPort != nil {
				out.ListenPort = *svc.ListenPort
			}
			if svc.DestinationPort != nil {
				out.DestinationPort = *svc.DestinationPort
			}
			if svc.HTTP != nil {
				out.HTTP = &schema.LoadBalancerServiceHTTP{}
				if svc.HTTP.Certificates != nil {
					out.HTTP.Certificates = *svc.HTTP.Certificates
				}
			}
			lb.Services = append(lb.Services, out)
		}
		f.lbs[lb.ID] = lb
		f.reply(w, http.StatusCreated, schema.LoadBalancerCreateResponse{LoadBalancer: *lb, Action: f.action(key, "create_load_balancer")})
	case r.Method == http.MethodGet && len(parts) == 2:
		if lb, ok := f.lbs[id]; ok {
			f.reply(w, http.StatusOK, schema.LoadBalancerGetResponse{LoadBalancer: *lb})
			return
		}
		f.refuse(w, http.StatusNotFound, "not_found", "load balancer not found")
	case r.Method == http.MethodDelete && len(parts) == 2:
		delete(f.lbs, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "update_service":
		lb, ok := f.lbs[id]
		if !ok {
			f.refuse(w, http.StatusNotFound, "not_found", "load balancer not found")
			return
		}
		req := decode[schema.LoadBalancerActionUpdateServiceRequest](f, r)
		for i := range lb.Services {
			if lb.Services[i].ListenPort == req.ListenPort && req.HTTP != nil && req.HTTP.Certificates != nil {
				if len(*req.HTTP.Certificates) == 0 {
					f.refuse(w, http.StatusUnprocessableEntity, "invalid_input", "https service requires a certificate")
					return
				}
				lb.Services[i].HTTP.Certificates = *req.HTTP.Certificates
			}
		}
		f.reply(w, http.StatusCreated, schema.LoadBalancerActionUpdateServiceResponse{Action: f.action(key, "update_service")})
	default:
		f.t.Errorf("fake cloud: unexpected request %s", key)
		f.refuse(w, http.StatusNotFound, "not_found", key)
	}
}
