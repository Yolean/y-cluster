package gateway

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestClassesWithDNSHintIP(t *testing.T) {
	cases := []struct {
		name string
		json string
		want []string
	}{
		{
			name: "empty list",
			json: `{"items":[]}`,
			want: nil,
		},
		{
			name: "annotation on default and custom class, absent on third",
			json: `{"items":[
				{"metadata":{"name":"y-cluster","annotations":{"yolean.se/dns-hint-ip":"192.0.2.10"}}},
				{"metadata":{"name":"eg","annotations":{"yolean.se/dns-hint-ip":"192.0.2.11","other":"x"}}},
				{"metadata":{"name":"istio","annotations":{"other":"x"}}}
			]}`,
			want: []string{"y-cluster", "eg"},
		},
		{
			name: "no annotations field at all",
			json: `{"items":[{"metadata":{"name":"bare"}}]}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classesWithDNSHintIP([]byte(tc.json))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	if _, err := classesWithDNSHintIP([]byte("not json")); err == nil {
		t.Error("expected error on invalid json")
	}
}

func TestIsNoResourceType(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		// kubectl's classic wording when the apiserver doesn't
		// serve the type, wrapped the way runKubectl wraps it.
		{fmt.Errorf("kubectl get gatewayclass -o json: error: the server doesn't have a resource type \"gatewayclass\"\n: exit status 1"), true},
		// discovery-mapper wording
		{errors.New(`no matches for kind "GatewayClass" in version "gateway.networking.k8s.io/v1"`), true},
		{errors.New("connection refused"), false},
		{errors.New(`gatewayclasses.gateway.networking.k8s.io "y-cluster" not found`), false},
	}
	for _, tc := range cases {
		if got := isNoResourceType(tc.err); got != tc.want {
			t.Errorf("isNoResourceType(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
