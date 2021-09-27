package config

import (
	"sync"

	"github.com/DataDog/datadog-agent/pkg/proto/pbgo"
)

// Store stores config provided by agent-core and returns
// latest configs to tracers.
type Store struct {
	mu      sync.RWMutex
	configs pbgo.ConfigResponse
}

// NewStore returns a new configuration store
func NewStore() *Store {
	return &Store{}
}

// Get returns the latest configuration for a product
func (s *Store) Get(req pb.ConfigRequest) (*pbgo.ConfigResponse, error) {
	if req.Product != pbgo.Product_LIVE_DEBUGGING {
		return nil, errNotAllowed
	}

	if !ok {
		err := s.newSubscriber(product)
		return nil, err
	}
	return &cfg, nil
}

// Stop listening for new configurations
func (s *Store) Stop() {
	s.stopSubscriber()
}
