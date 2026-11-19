package readyz

import (
	"io"
	"net/http"
	"testing"
)

func TestReadyzWithoutControlPlane(t *testing.T) {
	t.Parallel()

	srv, err := Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	resp, err := http.Get("http://" + srv.Addr().String() + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
}

func TestReadyzOtherPath(t *testing.T) {
	t.Parallel()

	srv, err := Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	resp, err := http.Get("http://" + srv.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /healthz status = %d, want 404", resp.StatusCode)
	}
}
