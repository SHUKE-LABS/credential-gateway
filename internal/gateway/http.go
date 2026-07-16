package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"credential-gateway/internal/config"
)

type httpProxy struct {
	cfg    config.HTTPService
	log    *slog.Logger
	server *http.Server
	addr   string // actual listening address (set in Start)
}

func (p *httpProxy) Start() error {
	upstream, err := url.Parse(p.cfg.Upstream)
	if err != nil {
		return fmt.Errorf("http proxy %s: invalid upstream %q: %w", p.cfg.Name, p.cfg.Upstream, err)
	}
	rp := httputil.NewSingleHostReverseProxy(upstream)
	origDirector := rp.Director
	headers := p.cfg.Headers
	rp.Director = func(req *http.Request) {
		origDirector(req)
		// origDirector points the dial target and TLS SNI at the upstream but
		// leaves req.Host as the client's value, which net/http sends as the
		// outbound Host header. Make the Host header agree with the upstream so
		// Host-routed backends (e.g. Cloudflare-fronted APIs) don't 403.
		req.Host = upstream.Host
		for k, v := range headers {
			req.Header.Del(k)
			req.Header.Set(k, v)
		}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.log.Error("http proxy upstream error", "name", p.cfg.Name, "path", r.URL.Path, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}
	rp.FlushInterval = -1
	ln, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return fmt.Errorf("http proxy %s: listen %s: %w", p.cfg.Name, p.cfg.Listen, err)
	}
	p.addr = ln.Addr().String()
	p.server = &http.Server{Handler: rp}
	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.Error("http proxy exited", "name", p.cfg.Name, "err", err)
		}
	}()
	return nil
}

func (p *httpProxy) Stop(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	return p.server.Shutdown(ctx)
}
