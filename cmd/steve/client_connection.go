package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type consoleConnection struct {
	URL   string
	Token string
}

type consoleClientFlags struct {
	configPath string
	url        string
	token      string
}

func addConsoleClientFlags(flags *flag.FlagSet) *consoleClientFlags {
	options := &consoleClientFlags{}
	flags.StringVar(&options.configPath, "config", "", "read console connection from this config file (read-only)")
	flags.StringVar(&options.url, "url", defaultReadModelURL, "read model URL of a running gateway")
	flags.StringVar(&options.token, "token", "", "token, when the read model is not on loopback")
	return options
}

func (options *consoleClientFlags) resolve(flags *flag.FlagSet) (consoleConnection, error) {
	explicit := make(map[string]bool)
	flags.Visit(func(entry *flag.Flag) { explicit[entry.Name] = true })
	connection := consoleConnection{URL: defaultReadModelURL}
	if explicit["config"] {
		var err error
		connection, err = readConsoleConnection(options.configPath)
		if err != nil {
			return consoleConnection{}, err
		}
	}
	if explicit["url"] {
		address, err := normalizeConsoleURL(options.url, false)
		if err != nil {
			return consoleConnection{}, fmt.Errorf("-url: %w", err)
		}
		original, _ := url.Parse(connection.URL)
		override, _ := url.Parse(address)
		if connection.Token != "" && !explicit["token"] && consoleOrigin(original) != consoleOrigin(override) {
			return consoleConnection{}, errors.New("-url changes the configured origin; supply an explicit -token (or -token '' to connect without credentials)")
		}
		connection.URL = address
	}
	if explicit["token"] {
		connection.Token = options.token
	}
	return connection, nil
}

func readConsoleConnection(path string) (consoleConnection, error) {
	var config struct {
		Gateway struct {
			Address string `json:"read_model_addr"`
			Token   string `json:"read_model_token"`
		} `json:"gateway"`
	}
	if err := readConsoleJSON(path, &config); err != nil {
		return consoleConnection{}, err
	}
	address, err := normalizeConsoleURL(config.Gateway.Address, true)
	if err != nil {
		return consoleConnection{}, fmt.Errorf("config gateway.read_model_addr: %w", err)
	}
	sidecarPath := path + ".cluster.json"
	if _, err := os.Lstat(sidecarPath); err == nil {
		var sidecar struct {
			UIAddress string `json:"ui_address"`
		}
		if err := readConsoleJSON(sidecarPath, &sidecar); err != nil {
			return consoleConnection{}, err
		}
		address, err = normalizeConsoleURL(sidecar.UIAddress, true)
		if err != nil {
			return consoleConnection{}, fmt.Errorf("cluster sidecar ui_address: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return consoleConnection{}, fmt.Errorf("inspect cluster sidecar: %w", err)
	}
	return consoleConnection{URL: address, Token: config.Gateway.Token}, nil
}

func readConsoleJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read console connection config: %w", err)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || json.Unmarshal(data, target) != nil {
		return fmt.Errorf("decode console connection config %s: expected a JSON object with valid connection fields", path)
	}
	return nil
}

func normalizeConsoleURL(address string, listeningAddress bool) (string, error) {
	invalid := errors.New("expected an HTTP(S) URL with a valid host and port, without userinfo, query or fragment")
	if address == "" && listeningAddress {
		return defaultReadModelURL, nil
	}
	if listeningAddress && !strings.Contains(address, "://") {
		if parsed, err := netip.ParseAddr(address); err == nil && parsed.Is6() {
			address = "[" + address + "]"
		}
		address = "http://" + address
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(address, "#") {
		return "", invalid
	}
	host, port := parsed.Hostname(), parsed.Port()
	if strings.HasSuffix(parsed.Host, ":") || (host == "" && port == "") {
		return "", invalid
	}
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", invalid
		}
	}
	addressIP, ipErr := netip.ParseAddr(host)
	if strings.ContainsAny(parsed.Host, "[]") || strings.Contains(host, ":") {
		if ipErr != nil || !addressIP.Is6() || !strings.HasPrefix(parsed.Host, "[") {
			return "", invalid
		}
	}
	if host == "" || (ipErr == nil && addressIP.IsUnspecified()) {
		host = "127.0.0.1"
		if ipErr == nil && addressIP.Is6() {
			host = "::1"
		}
		if port != "" {
			parsed.Host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			parsed.Host = "[" + host + "]"
		} else {
			parsed.Host = host
		}
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func consoleOrigin(address *url.URL) string {
	host := strings.ToLower(address.Hostname())
	if parsed, err := netip.ParseAddr(address.Hostname()); err == nil {
		host = parsed.String()
	}
	port := address.Port()
	if port == "" {
		port = "80"
		if address.Scheme == "https" {
			port = "443"
		}
	} else if number, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(number)
	}
	return address.Scheme + "://" + net.JoinHostPort(host, port)
}

func checkConsoleRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if request.URL.User != nil || consoleOrigin(request.URL) != consoleOrigin(via[0].URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

func consoleHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: checkConsoleRedirect}
}
