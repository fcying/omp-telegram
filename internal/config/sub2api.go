package config

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Sub2API enables bridge-only upstream quota for exact OMP provider IDs.
type Sub2API struct {
	Extension bool     `toml:"extension"`
	BaseURL   string   `toml:"base_url"`
	APIKey    string   `toml:"api_key"`
	Providers []string `toml:"providers"`
}

func (c Sub2API) Enabled() bool {
	return c.BaseURL != "" && c.APIKey != "" && len(c.Providers) != 0
}

func (c Sub2API) HasProvider(provider string) bool {
	if !c.Enabled() {
		return false
	}
	for _, configured := range c.Providers {
		if configured == provider {
			return true
		}
	}
	return false
}

func prepareSub2API(raw Sub2API) (Sub2API, error) {
	c := Sub2API{Extension: raw.Extension}
	var err error
	if c.BaseURL, err = expand(raw.BaseURL); err != nil {
		return Sub2API{}, errors.New("sub2api.base_url has an invalid or unset environment reference")
	}
	if c.APIKey, err = expand(raw.APIKey); err != nil {
		return Sub2API{}, errors.New("sub2api.api_key has an invalid or unset environment reference")
	}
	for _, provider := range raw.Providers {
		provider, err = expand(provider)
		if err != nil {
			return Sub2API{}, errors.New("sub2api.providers has an invalid or unset environment reference")
		}
		c.Providers = append(c.Providers, provider)
	}
	if c.BaseURL == "" && c.APIKey == "" && len(c.Providers) == 0 {
		return c, nil
	}
	if !c.Enabled() {
		return Sub2API{}, errors.New("sub2api requires nonempty base_url, api_key and providers")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(c.BaseURL, "?#\\") || strings.IndexFunc(c.BaseURL, unicode.IsSpace) >= 0 || strings.IndexFunc(c.BaseURL, unicode.IsControl) >= 0 {
		return Sub2API{}, errors.New("sub2api.base_url must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return Sub2API{}, errors.New("sub2api.base_url has an invalid port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Sub2API{}, errors.New("sub2api.base_url has an invalid port")
		}
	}
	if strings.ContainsAny(u.Path, "\\") || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return Sub2API{}, errors.New("sub2api.base_url has an invalid path")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return Sub2API{}, errors.New("sub2api.base_url has an invalid path")
		}
	}
	if strings.TrimSpace(c.APIKey) != c.APIKey || strings.IndexFunc(c.APIKey, unicode.IsControl) >= 0 {
		return Sub2API{}, errors.New("sub2api.api_key must be a nonempty single-line credential")
	}
	for i, provider := range c.Providers {
		if provider == "" || strings.IndexFunc(provider, unicode.IsSpace) >= 0 || strings.IndexFunc(provider, unicode.IsControl) >= 0 || strings.ContainsAny(provider, "/\\") {
			return Sub2API{}, errors.New("sub2api.providers must contain exact nonempty OMP provider IDs")
		}
		for _, previous := range c.Providers[:i] {
			if provider == previous {
				return Sub2API{}, errors.New("sub2api.providers must not contain duplicate IDs")
			}
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}
