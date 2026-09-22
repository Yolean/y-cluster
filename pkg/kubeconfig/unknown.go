package kubeconfig

import (
	"encoding/json"
	"reflect"
	"strings"
)

// The kubeconfig in $KUBECONFIG belongs to the operator. y-cluster
// adds and removes its own entries and has to write everything else
// back as it found it, including fields these types do not model
// (proxy-url, tls-server-name, as-groups, extensions written by kubie
// or minikube, whatever a future kubectl adds). Every type that a
// load/save round trip passes through therefore keeps the keys it
// does not know in an unexported map and re-emits them.

// decodeKeepingUnknown unmarshals data into typed (a pointer to a
// struct) and returns the top-level keys that struct has no json
// field for.
func decodeKeepingUnknown(data []byte, typed any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, typed); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for _, known := range jsonFieldNames(reflect.TypeOf(typed).Elem()) {
		delete(all, known)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// encodeWithUnknown marshals typed and adds the unknown keys back. A
// key the struct models always wins.
func encodeWithUnknown(typed any, unknown map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(typed)
	if err != nil || len(unknown) == 0 {
		return data, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, v := range unknown {
		if _, modelled := merged[k]; !modelled {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

func (f *File) UnmarshalJSON(data []byte) error {
	type plain File
	var p plain
	unknown, err := decodeKeepingUnknown(data, &p)
	if err != nil {
		return err
	}
	*f = File(p)
	f.unknown = unknown
	return nil
}

func (f File) MarshalJSON() ([]byte, error) {
	type plain File
	return encodeWithUnknown(plain(f), f.unknown)
}

func (c *Cluster) UnmarshalJSON(data []byte) error {
	type plain Cluster
	var p plain
	unknown, err := decodeKeepingUnknown(data, &p)
	if err != nil {
		return err
	}
	*c = Cluster(p)
	c.unknown = unknown
	return nil
}

func (c Cluster) MarshalJSON() ([]byte, error) {
	type plain Cluster
	return encodeWithUnknown(plain(c), c.unknown)
}

func (c *Context) UnmarshalJSON(data []byte) error {
	type plain Context
	var p plain
	unknown, err := decodeKeepingUnknown(data, &p)
	if err != nil {
		return err
	}
	*c = Context(p)
	c.unknown = unknown
	return nil
}

func (c Context) MarshalJSON() ([]byte, error) {
	type plain Context
	return encodeWithUnknown(plain(c), c.unknown)
}

func (u *User) UnmarshalJSON(data []byte) error {
	type plain User
	var p plain
	unknown, err := decodeKeepingUnknown(data, &p)
	if err != nil {
		return err
	}
	*u = User(p)
	u.unknown = unknown
	return nil
}

func (u User) MarshalJSON() ([]byte, error) {
	type plain User
	return encodeWithUnknown(plain(u), u.unknown)
}
