package model

import "github.com/traefik/traefik/v3/pkg/advancedcache/config"

type Revalidator func(rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte) (
	status int, headers *[][2][]byte, body []byte, releaseFn func(), err error,
)
