package llamaserver

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientOpensNewConnectionPerRequest(t *testing.T) {
	const requests = 3

	var connections atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.Start()
	t.Cleanup(upstream.Close)

	for range requests {
		resp, err := client.Post(upstream.URL, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if got := connections.Load(); got != requests {
		t.Errorf("opened %d connections for %d requests, want one per request", got, requests)
	}
}
