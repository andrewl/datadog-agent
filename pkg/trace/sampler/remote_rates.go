package sampler

import (
	"sync"
	"time"

	"github.com/DataDog/datadog-agent/pkg/config/remote/service"
	"github.com/DataDog/datadog-agent/pkg/proto/pbgo"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/pb"
	"github.com/DataDog/datadog-agent/pkg/trace/watchdog"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// RemoteRates sharded by signature. Allowing an independant feedback
// loop per signature. RemoteRates adjusts sampling rates communicated
// to the agent based on the observed traffic. It targets a TPS configured
// remotely.
type RemoteRates struct {
	samplers   map[Signature]*Sampler
	tpsTargets map[Signature]float64
	mu         sync.RWMutex // protects concurrent access to samplers and tpsTargets

	exit    chan struct{}
	stopped chan struct{}
}

func newRemoteRates(conf *config.AgentConfig) *RemoteRates {
	if !conf.EnabledRemoteRates {
		return nil
	}

	remoteRates := &RemoteRates{
		samplers: make(map[Signature]*Sampler),

		exit:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	if err := service.NewGRPCSubscriber(pbgo.Product_APM_SAMPLING, remoteRates.subscriberCallback()); err != nil {
		log.Errorf("Error when subscribing to remote config management %v", err)
		return nil
	}
	return remoteRates
}

func (r *RemoteRates) subscriberCallback() func(config *pbgo.ConfigResponse) error {
	return func(config *pbgo.ConfigResponse) error {
		log.Infof("Fetched config version %d from remote config management", config.DirectoryTargets.Version)
		return r.loadNewConfig(config)
	}
}

func (r *RemoteRates) loadNewConfig(config *pbgo.ConfigResponse) error {
	tpsTargets := make(map[Signature]float64, len(r.tpsTargets))
	for _, targetFile := range config.TargetFiles {
		var rates pb.RemoteRates
		_, err := rates.UnmarshalMsg(targetFile.Raw)
		if err != nil {
			return err
		}
		for _, rate := range rates.Rates {
			sig := ServiceSignature{Name: rate.Service, Env: rate.Env}.Hash()
			tpsTargets[sig] = rate.Rate
		}
	}
	r.updateTPS(tpsTargets)
	return nil
}

func (r *RemoteRates) updateTPS(tpsTargets map[Signature]float64) {
	r.mu.Lock()
	r.tpsTargets = tpsTargets
	r.mu.Unlock()

	// update samplers with new TPS
	r.mu.RLock()
	noTPSConfigured := map[Signature]struct{}{}
	for sig, sampler := range r.samplers {
		rate, ok := tpsTargets[sig]
		if !ok {
			noTPSConfigured[sig] = struct{}{}
		}
		sampler.UpdateTargetTPS(rate)
	}
	r.mu.RUnlock()

	// trim signatures with no TPS configured
	r.mu.Lock()
	for sig := range noTPSConfigured {
		delete(r.samplers, sig)
	}
	r.mu.Unlock()
}

// Start runs and adjust rates per signature following remote TPS targets
func (r *RemoteRates) Start() {
	go func() {
		defer watchdog.LogOnPanic()
		decayTicker := time.NewTicker(defaultDecayPeriod)
		adjustTicker := time.NewTicker(adjustPeriod)
		statsTicker := time.NewTicker(10 * time.Second)
		defer decayTicker.Stop()
		defer adjustTicker.Stop()
		defer statsTicker.Stop()
		for {
			select {
			case <-decayTicker.C:
				r.DecayScores()
			case <-adjustTicker.C:
				r.AdjustScoring()
			case <-statsTicker.C:
				// todo:raphael report stats
				//r.report()
			case <-r.exit:
				close(r.stopped)
				return
			}
		}
	}()
}

// DecayScores decays scores of all samplers
func (r *RemoteRates) DecayScores() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.samplers {
		s.Backend.DecayScore()
	}
}

// AdjustScoring adjust scores of all samplers
func (r *RemoteRates) AdjustScoring() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.samplers {
		s.AdjustScoring()
	}
}

// Stop stops RemoteRates main loop
func (r *RemoteRates) Stop() {
	close(r.exit)
	<-r.stopped
}

func (r *RemoteRates) getSampler(sig Signature) (*Sampler, bool) {
	r.mu.RLock()
	s, ok := r.samplers[sig]
	r.mu.RUnlock()
	return s, ok
}

func (r *RemoteRates) initSampler(sig Signature) (*Sampler, bool) {
	r.mu.RLock()
	targetTPS, ok := r.tpsTargets[sig]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	s := newSampler(1.0, targetTPS, nil)
	r.mu.Lock()
	r.samplers[sig] = s
	r.mu.Unlock()
	return s, true
}

// CountSignature counts the number of root span seen matching a signature.
func (r *RemoteRates) CountSignature(sig Signature) {
	s, ok := r.getSampler(sig)
	if !ok {
		if s, ok = r.initSampler(sig); !ok {
			return
		}
	}
	s.Backend.CountSignature(sig)
}

// CountSample counts the number of sampled root span matching a signature.
func (r *RemoteRates) CountSample(sig Signature) {
	s, ok := r.getSampler(sig)
	if !ok {
		return
	}
	s.Backend.CountSample()
}

// CountWeightedSig counts weighted root span seen for a signature.
// This function is called when trace-agent client drop unsampled spans.
// as dropped root spans are not accounted anymore in CountSignature calls.
func (r *RemoteRates) CountWeightedSig(sig Signature, weight float64) {
	s, ok := r.getSampler(sig)
	if !ok {
		return
	}
	s.Backend.CountWeightedSig(sig, weight)
	s.Backend.AddTotalScore(weight)
}

// GetSignatureSampleRate returns the sampling rate to apply for a registered signature.
func (r *RemoteRates) GetSignatureSampleRate(sig Signature) (float64, bool) {
	s, ok := r.getSampler(sig)
	if !ok {
		return 0, false
	}
	return s.GetSignatureSampleRate(sig), true
}

// GetAllSignatureSampleRates returns sampling rates to apply for all registered signatures.
func (r *RemoteRates) GetAllSignatureSampleRates() map[Signature]float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make(map[Signature]float64, len(r.samplers))
	for sig, s := range r.samplers {
		res[sig] = s.GetSignatureSampleRate(sig)
	}
	return res
}
