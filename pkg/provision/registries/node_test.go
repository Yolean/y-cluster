package registries

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

func TestWriteToNode_EmptyWritesNothing(t *testing.T) {
	exec := func(context.Context, string, io.Reader) ([]byte, error) {
		t.Error("an empty registries config must not touch the node")
		return nil, nil
	}
	if err := WriteToNode(context.Background(), exec, config.Registries{}, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
}

func TestWriteToNode_PipesTheFileAsRootOnly(t *testing.T) {
	r := config.Registries{Mirrors: map[string]config.RegistryMirror{
		"docker.io": {Endpoint: []string{"http://mirror.local:5000"}},
	}}
	var command, body string
	exec := func(_ context.Context, cmd string, stdin io.Reader) ([]byte, error) {
		command = cmd
		b, _ := io.ReadAll(stdin)
		body = string(b)
		return nil, nil
	}
	if err := WriteToNode(context.Background(), exec, r, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	want, _ := Marshal(r)
	if body != string(want) {
		t.Errorf("stdin = %q, want the marshalled file %q", body, want)
	}
	// The file can hold registry credentials.
	if !strings.Contains(command, "-m 0600 /dev/stdin "+Path) {
		t.Errorf("command does not write %s as 0600: %s", Path, command)
	}
}

func TestWriteToNode_FailureCarriesNodeOutput(t *testing.T) {
	r := config.Registries{Mirrors: map[string]config.RegistryMirror{"docker.io": {}}}
	exec := func(context.Context, string, io.Reader) ([]byte, error) {
		return []byte("sudo: a password is required"), errors.New("exit status 1")
	}
	err := WriteToNode(context.Background(), exec, r, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "a password is required") {
		t.Errorf("error should carry node output: %v", err)
	}
}
