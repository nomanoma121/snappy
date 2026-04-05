package registory

import (
	"fmt"
)

const (
	GHCRHost = "ghcr.io"
	GCRHost  = "gcr.io"
)

type Registry interface {
	DockerConfig(appName string) string
	Token() string
}

type GHCR struct {
	host  string
	token string
}

func NewGHCR(host, token string) *GHCR {
	return &GHCR{
		host:  host,
		token: token,
	}
}

func (r *GHCR) DockerConfig(appName string) string {
	return fmt.Sprintf(`{"auths":{"%s":{"username":"x-access-token","password":%q}}}`, r.host, r.token)
}

func (r *GHCR) Token() string {
	return r.token
}

type GCR struct {
	host  string
	token string
}

func NewGCR(host, token string) *GCR {
	return &GCR{
		host:  host,
		token: token,
	}
}

func (r *GCR) DockerConfig(appName string) string {
	return fmt.Sprintf(`{"auths":{"%s":{"username":"_json_key","password":%q}}}`, r.host, r.token)
}

func (r *GCR) Token() string {
	return r.token
}
