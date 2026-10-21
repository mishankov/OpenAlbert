package integrations

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}
type WebConfig struct {
	// HTTPClient is an optional transport for contract tests; URL and DNS policy still applies.
	HTTPClient *http.Client
	Resolver   IPResolver
	MaxBytes   int64
	MaxText    int
}
type WebReader struct {
	client   *http.Client
	resolver IPResolver
	maxBytes int64
	maxText  int
}

func NewWebReader(c WebConfig) *WebReader {
	if c.Resolver == nil {
		c.Resolver = net.DefaultResolver
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 2 << 20
	}
	if c.MaxText <= 0 {
		c.MaxText = 64000
	}
	w := &WebReader{resolver: c.Resolver, maxBytes: c.MaxBytes, maxText: c.MaxText}
	if c.HTTPClient == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid web address")
			}
			ips, err := w.publicIPs(ctx, host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, ip := range ips {
				conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		}
		w.client = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	} else {
		copy := *c.HTTPClient
		w.client = &copy
		if w.client.Timeout <= 0 {
			w.client.Timeout = 30 * time.Second
		}
	}
	w.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return w.validate(req.Context(), req.URL)
	}
	return w
}

var nonPublic = []*net.IPNet{}

func init() {
	for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "2001:db8::/32", "2001::/23", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		nonPublic = append(nonPublic, block)
	}
}
func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, n := range nonPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
func (w *WebReader) publicIPs(ctx context.Context, host string) ([]net.IPAddr, error) {
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else {
		var err error
		ips, err = w.resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, errors.New("web DNS lookup failed")
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("web host has no addresses")
	}
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			return nil, errors.New("web host resolves to a non-public address")
		}
	}
	return ips, nil
}
func (w *WebReader) validate(ctx context.Context, u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return errors.New("web URL must be public HTTP(S) without credentials")
	}
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		return errors.New("web URL port must be 80 or 443")
	}
	_, err := w.publicIPs(ctx, u.Hostname())
	return err
}
func (w *WebReader) Read(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid web URL")
	}
	if err = w.validate(ctx, u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errors.New("invalid web URL")
	}
	req.Header.Set("User-Agent", "Jarvis/1.0 (+public-page-reader)")
	req.Header.Set("Accept", "text/html, text/plain;q=0.9")
	resp, err := w.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &RequestError{Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &HTTPError{Status: resp.StatusCode, Source: "web"}
	}
	contentType := strings.ToLower(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if contentType != "text/html" && contentType != "application/xhtml+xml" && contentType != "text/plain" {
		return "", errors.New("web response is not HTML or plain text")
	}
	b, err := limitedRead(resp.Body, w.maxBytes)
	if err != nil {
		return "", err
	}
	text := string(b)
	if contentType != "text/plain" {
		text = readableHTML(text)
	} else {
		text = strings.Join(strings.Fields(text), " ")
	}
	if len(text) > w.maxText {
		text = text[:w.maxText]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "\n[Truncated]"
	}
	return text, nil
}
func readableHTML(input string) string {
	root, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return ""
	}
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "svg", "nav", "footer", "header", "form", "iframe":
				return
			}
			for _, a := range n.Attr {
				if a.Key == "hidden" || (a.Key == "aria-hidden" && a.Val == "true") {
					return
				}
			}
		}
		if n.Type == html.TextNode {
			if s := strings.Join(strings.Fields(n.Data), " "); s != "" {
				parts = append(parts, s)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return strings.Join(parts, " ")
}

// DialPublic is shared by the browser egress proxy. It connects to a checked IP
// rather than resolving again after validation, including for HTTPS tunnels.
func DialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || (port != "80" && port != "443") {
		return nil, errors.New("web port must be 80 or 443")
	}
	w := &WebReader{resolver: net.DefaultResolver}
	ips, err := w.publicIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range ips {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
func ValidatePublicURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid web URL")
	}
	return (&WebReader{resolver: net.DefaultResolver}).validate(ctx, u)
}
