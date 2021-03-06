package stress

import (
	"fmt"
	"sync"
	"time"

	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/common/atomic"
	"github.com/elastic/beats/libbeat/logp"
)

type generateConfig struct {
	Worker      int           `config:"worker" validate:"min=1"`
	ACK         bool          `config:"ack"`
	MaxEvents   uint64        `config:"max_events"`
	WaitClose   time.Duration `config:"wait_close"`
	PublishMode string        `config:"publish_mode"`
	Watchdog    time.Duration `config:"watchdog"`
}

var defaultGenerateConfig = generateConfig{
	Worker:    1,
	ACK:       false,
	MaxEvents: 0,
	WaitClose: 0,
	Watchdog:  1 * time.Second,
}

var publishModes = map[string]beat.PublishMode{
	"":             beat.DefaultGuarantees,
	"default":      beat.DefaultGuarantees,
	"guaranteed":   beat.GuaranteedSend,
	"drop_if_full": beat.DropIfFull,
}

func generate(
	cs *closeSignaler,
	p beat.Pipeline,
	config generateConfig,
	id int,
	errors func(err error),
) error {
	settings := beat.ClientConfig{
		WaitClose: config.WaitClose,
	}

	if config.ACK {
		settings.ACKCount = func(n int) {
			logp.Info("Pipeline client (%v) ACKS; %v", id, n)
		}
	}

	if m := config.PublishMode; m != "" {
		mode, exists := publishModes[m]
		if !exists {
			err := fmt.Errorf("Unknown publish mode '%v'", mode)
			if errors != nil {
				errors(err)
			}
			return err
		}

		settings.PublishMode = mode
	}

	client, err := p.ConnectWith(settings)
	if err != nil {
		panic(err)
	}

	defer logp.Info("client (%v) closed: %v", id, time.Now())

	done := make(chan struct{})
	defer close(done)

	count := atomic.MakeUint64(0)

	var wg sync.WaitGroup
	defer wg.Wait()
	withWG(&wg, func() {
		select {
		case <-cs.C(): // stop signal has been received
		case <-done: // generate just returns
		}

		client.Close()
	})

	if errors != nil && config.Watchdog > 0 {
		// start generator watchdog
		withWG(&wg, func() {
			last := uint64(0)
			ticker := time.NewTicker(config.Watchdog) // todo: make ticker interval configurable
			defer ticker.Stop()
			for {
				select {
				case <-cs.C():
					return
				case <-done:
					return
				case <-ticker.C:
				}

				current := count.Load()
				if last == current {
					err := fmt.Errorf("no progress in generators (last=%v, current=%v)", last, current)
					errors(err)
				}
				last = current
			}
		})
	}

	logp.Info("start (%v) generator: %v", id, time.Now())
	defer logp.Info("stop (%v) generator: %v", id, time.Now())

	for cs.Active() {
		event := beat.Event{
			Timestamp: time.Now(),
			Fields: common.MapStr{
				"id":    id,
				"hello": "world",
				"count": count,

				// TODO: more custom event generation?
			},
		}

		client.Publish(event)

		total := count.Inc()
		if config.MaxEvents > 0 && total == config.MaxEvents {
			break
		}
	}

	return nil
}
