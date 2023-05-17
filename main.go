package main

import (
	"fmt"
	"image/jpeg"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kolesa-team/go-webp/encoder"
	"github.com/kolesa-team/go-webp/webp"
	"github.com/quic-go/quic-go/http3"
)

// http/3 client
var h3client = &http.Client{
	Transport: &http3.RoundTripper{},
}

var dialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// http/2 client
var h2client = &http.Client{
	Transport: &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			if disable_ipv6 {
				network = "tcp4"
			}
			return dialer.Dial(network, addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ReadBufferSize:        16 * 1024,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       0,
		MaxIdleConnsPerHost:   10,
		MaxIdleConns:          0,
	},
}

// user agent to use
var ua = "Mozilla/5.0 (Windows NT 10.0; rv:78.0) Gecko/20100101"

var allowed_hosts = []string{
	"youtube.com",
	"googlevideo.com",
	"ytimg.com",
	"ggpht.com",
	"googleusercontent.com",
	"lbryplayer.xyz",
	"odycdn.com",
}

var strip_headers = []string{
	"Accept-Encoding",
	"Authorization",
	"Origin",
	"Referer",
	"Cookie",
	"Set-Cookie",
	"Etag",
}

var path_prefix = ""

var manifest_re = regexp.MustCompile(`(?m)URI="([^"]+)"`)

var disable_ipv6 = false
var disable_webp = false

type requesthandler struct{}

func (*requesthandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Max-Age", "1728000")

	if req.Method == "OPTIONS" {
		w.WriteHeader(200)
		return
	}

	q := req.URL.Query()
	host := q.Get("host")
	q.Del("host")

	if len(host) <= 0 {
		host = q.Get("hls_chunk_host")
	}

	if len(host) <= 0 {
		host = getHost(req.URL.EscapedPath())
	}

	if len(host) <= 0 {
		io.WriteString(w, "No host in query parameters.")
		return
	}

	parts := strings.Split(strings.ToLower(host), ".")

	if len(parts) < 2 {
		io.WriteString(w, "Invalid hostname.")
		return
	}

	domain := parts[len(parts)-2] + "." + parts[len(parts)-1]

	disallowed := true

	for _, value := range allowed_hosts {
		if domain == value {
			disallowed = false
			break
		}
	}

	if disallowed {
		io.WriteString(w, "Non YouTube domains are not supported.")
		return
	}

	if req.Method != "GET" && req.Method != "HEAD" {
		io.WriteString(w, "Only GET and HEAD requests are allowed.")
		return
	}

	path := req.URL.EscapedPath()

	path = strings.Replace(path, "/ggpht", "", 1)
	path = strings.Replace(path, "/i/", "/", 1)

	proxyURL, err := url.Parse("https://" + host + path)

	if err != nil {
		log.Panic(err)
	}

	proxyURL.RawQuery = q.Encode()

	if strings.HasSuffix(proxyURL.EscapedPath(), "maxres.jpg") {
		proxyURL.Path = getBestThumbnail(proxyURL.EscapedPath())
	}

	request, err := http.NewRequest(req.Method, proxyURL.String(), nil)

	copyHeaders(req.Header, request.Header, false)
	request.Header.Set("User-Agent", ua)

	if err != nil {
		log.Panic(err)
	}

	var client *http.Client

	// https://github.com/lucas-clemente/quic-go/issues/2836
	client = h2client

	resp, err := client.Do(request)

	if err != nil {
		log.Panic(err)
	}

	defer resp.Body.Close()

	NoRewrite := strings.HasPrefix(resp.Header.Get("Content-Type"), "audio") || strings.HasPrefix(resp.Header.Get("Content-Type"), "video") || strings.HasPrefix(resp.Header.Get("Content-Type"), "webp")
	copyHeaders(resp.Header, w.Header(), NoRewrite)

	w.WriteHeader(resp.StatusCode)

	if req.Method == "GET" && (resp.Header.Get("Content-Type") == "application/x-mpegurl" || resp.Header.Get("Content-Type") == "application/vnd.apple.mpegurl") {
		bytes, err := io.ReadAll(resp.Body)

		if err != nil {
			log.Panic(err)
		}

		lines := strings.Split(string(bytes), "\n")
		reqUrl := resp.Request.URL
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			if !strings.HasPrefix(line, "https://") && (strings.HasSuffix(line, ".m3u8") || strings.HasSuffix(line, ".ts")) {
				path := reqUrl.EscapedPath()
				path = path[0 : strings.LastIndex(path, "/")+1]
				line = "https://" + reqUrl.Hostname() + path + line
			}
			if strings.HasPrefix(line, "https://") {
				lines[i] = RelativeUrl(line)
			}

			if manifest_re.MatchString(line) {
				url := manifest_re.FindStringSubmatch(line)[1]
				lines[i] = strings.Replace(line, url, RelativeUrl(url), 1)
			}
		}

		io.WriteString(w, strings.Join(lines, "\n"))
	} else if !disable_webp && resp.Header.Get("Content-Type") == "image/jpeg" {
		img, err := jpeg.Decode(resp.Body)

		if err != nil {
			log.Panic(err)
		}

		options, _ := encoder.NewLossyEncoderOptions(encoder.PresetDefault, 85)

		w.Header().Set("Content-Type", "image/webp")

		webp.Encode(w, img, options)
	} else {
		io.Copy(w, resp.Body)
	}
}

func copyHeaders(from http.Header, to http.Header, length bool) {
	// Loop over header names
outer:
	for name, values := range from {
		for _, header := range strip_headers {
			if name == header {
				continue outer
			}
		}
		if (name != "Content-Length" || length) && !strings.HasPrefix(name, "Access-Control") {
			// Loop over all values for the name.
			for _, value := range values {
				if strings.Contains(value, "jpeg") {
					continue
				}
				to.Set(name, value)
			}
		}
	}
}

func getHost(path string) (host string) {

	host = ""

	if strings.HasPrefix(path, "/vi/") || strings.HasPrefix(path, "/vi_webp/") || strings.HasPrefix(path, "/sb/") {
		host = "i.ytimg.com"
	}

	if strings.HasPrefix(path, "/ggpht/") {
		host = "yt3.ggpht.com"
	}

	if strings.HasPrefix(path, "/a/") || strings.HasPrefix(path, "/ytc/") {
		host = "yt3.ggpht.com"
	}

	if strings.Contains(path, "/host/") {
		path = path[(strings.Index(path, "/host/") + 6):]
		host = path[0:strings.Index(path, "/")]
	}

	return host
}

func getBestThumbnail(path string) (newpath string) {

	formats := [4]string{"maxresdefault.jpg", "sddefault.jpg", "hqdefault.jpg", "mqdefault.jpg"}

	for _, format := range formats {
		newpath = strings.Replace(path, "maxres.jpg", format, 1)
		url := "https://i.ytimg.com" + newpath
		resp, _ := h2client.Head(url)
		if resp.StatusCode == 200 {
			return newpath
		}
	}

	return strings.Replace(path, "maxres.jpg", "mqdefault.jpg", 1)
}

func RelativeUrl(in string) (newurl string) {
	segment_url, err := url.Parse(in)
	if err != nil {
		log.Panic(err)
	}
	segment_query := segment_url.Query()
	segment_query.Set("host", segment_url.Hostname())
	segment_url.RawQuery = segment_query.Encode()
	segment_url.Path = path_prefix + segment_url.Path
	return segment_url.RequestURI()
}

func main() {
	path_prefix = os.Getenv("PREFIX_PATH")

	disable_ipv6 = os.Getenv("DISABLE_IPV6") == "1"
	disable_webp = os.Getenv("DISABLE_WEBP") == "1"

	socket := "socket" + string(os.PathSeparator) + "http-proxy.sock"
	syscall.Unlink(socket)
	listener, err := net.Listen("unix", socket)
	srv := &http.Server{
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 1 * time.Hour,
		Addr:         ":8080",
		Handler:      &requesthandler{},
	}
	if err != nil {
		fmt.Println("Failed to bind to UDS, falling back to TCP/IP")
		fmt.Println(err.Error())
		srv.ListenAndServe()
	} else {
		defer listener.Close()
		srv.Serve(listener)
	}
}
