package sampler

import (
	"testing"

	"github.com/DataDog/datadog-agent/pkg/proto/pbgo"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteConfInit(t *testing.T) {
	assert := assert.New(t)
	// disabled by default
	assert.Nil(newRemoteRates(&config.AgentConfig{}))
	// subscription to subscriber fails
	assert.Nil(newRemoteRates(&config.AgentConfig{EnabledRemoteRates: true}))
	// todo:raphael mock grpc server
}

func newTestRemoteRates() *RemoteRates {
	return &RemoteRates{
		samplers: make(map[Signature]*Sampler),

		exit:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func configGenerator(rates pb.RemoteRates) *pbgo.ConfigResponse {
	raw, _ := rates.MarshalMsg(nil)
	return &pbgo.ConfigResponse{
		TargetFiles: []*pbgo.File{{Raw: raw}},
	}
}

func TestRemoteTPSUpdate(t *testing.T) {
	assert := assert.New(t)

	type sampler struct {
		service   string
		targetTPS float64
	}

	var testSteps = []struct {
		name             string
		ratesToApply     pb.RemoteRates
		countServices    []string
		expectedSamplers []sampler
	}{
		{
			name: "first rates received",
			ratesToApply: pb.RemoteRates{
				Rates: []pb.Rate{
					{
						Service: "willBeRemoved1",
						Rate:    3.2,
					},
					{
						Service: "willBeRemoved2",
						Rate:    33,
					},
					{
						Service: "keep",
						Rate:    1,
					},
				},
			},
		},
		{
			name: "enable a sampler after counting a matching service",
			countServices: []string{
				"willBeRemoved1",
			},
			expectedSamplers: []sampler{
				{
					service:   "willBeRemoved1",
					targetTPS: 3.2,
				},
			},
		},
		{
			name: "nothing happens when counting a service not set remotely",
			countServices: []string{
				"no remote tps",
			},
			expectedSamplers: []sampler{
				{
					service:   "willBeRemoved1",
					targetTPS: 3.2,
				},
			},
		},
		{
			name: "add 2 more samplers",
			countServices: []string{
				"keep",
				"willBeRemoved2",
			},
			expectedSamplers: []sampler{
				{
					service:   "willBeRemoved1",
					targetTPS: 3.2,
				},
				{
					service:   "willBeRemoved2",
					targetTPS: 33,
				},
				{
					service:   "keep",
					targetTPS: 1,
				},
			},
		},
		{
			name: "receive new remote rates, non matching samplers are trimmed",
			ratesToApply: pb.RemoteRates{
				Rates: []pb.Rate{
					{
						Service: "keep",
						Rate:    27,
					},
				},
			},
			expectedSamplers: []sampler{
				{
					service:   "keep",
					targetTPS: 27,
				},
			},
		},
	}
	r := newTestRemoteRates()
	for _, step := range testSteps {
		t.Log(step.name)
		if step.ratesToApply.Rates != nil {
			r.loadNewConfig(configGenerator(step.ratesToApply))
		}
		for _, s := range step.countServices {
			r.CountSignature(ServiceSignature{Name: s}.Hash())
		}

		assert.Len(r.samplers, len(step.expectedSamplers))

		for _, expectedS := range step.expectedSamplers {
			s, ok := r.samplers[ServiceSignature{Name: expectedS.service}.Hash()]
			require.True(t, ok)
			assert.Equal(expectedS.targetTPS, s.targetTPS.Load())
		}
	}
}
