package amp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRootManagementAuthBypassRequiresLoopbackRestriction(t *testing.T) {
	gin.SetMode(gin.TestMode)

	m := &AmpModule{}
	m.setRestrictToLocalhost(false)

	proxyCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy, err := createReverseProxy(upstream.URL, NewStaticSecretSource(""))
	if err != nil {
		t.Fatal(err)
	}
	m.setProxy(proxy)

	authCalled := false
	auth := func(c *gin.Context) {
		authCalled = true
		c.AbortWithStatus(http.StatusUnauthorized)
	}

	r := gin.New()
	m.registerManagementRoutes(r, nil, auth)

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/threads")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !authCalled {
		t.Fatal("auth middleware should be required when localhost restriction is disabled")
	}
	if proxyCalled {
		t.Fatal("proxy should not be reached after auth aborts")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRootManagementAuthBypassAllowedForLoopbackWhenRestricted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	m := &AmpModule{}
	m.setRestrictToLocalhost(true)

	proxyCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	proxy, err := createReverseProxy(upstream.URL, NewStaticSecretSource(""))
	if err != nil {
		t.Fatal(err)
	}
	m.setProxy(proxy)

	authCalled := false
	auth := func(c *gin.Context) {
		authCalled = true
		c.AbortWithStatus(http.StatusUnauthorized)
	}

	r := gin.New()
	m.registerManagementRoutes(r, nil, auth)

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/threads")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if authCalled {
		t.Fatal("auth middleware should be bypassed for selected loopback management routes")
	}
	if !proxyCalled {
		t.Fatal("proxy should be reached")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}
