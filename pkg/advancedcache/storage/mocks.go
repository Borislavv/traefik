package storage

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/mock"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
)

func LoadMocks(ctx context.Context, config *config.Cache, backend repository.Backender, storage Storage, num int) {
	go func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()

		log.Info().Msg("[mocks] mock data start loading")
		defer log.Info().Msg("[mocks] mocked data finished loading")

		path := []byte("/api/v2/pagedata")
		for entry := range mock.StreamEntryPointersConsecutive(ctx, config, backend, path, num) {
			storage.Set(entry)
		}
	}()
}
