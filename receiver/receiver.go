package receiver

import (
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/Nitro/sidecar/catalog"
	"github.com/Nitro/sidecar/service"
	log "github.com/Sirupsen/logrus"
)

const (
	RELOAD_HOLD_DOWN = 5 * time.Second // Reload at worst every 5 seconds
)

type Receiver struct {
	StateLock      sync.Mutex
	ReloadChan     chan time.Time
	CurrentState   *catalog.ServicesState
	LastSvcChanged *service.Service
	OnUpdate       func(state *catalog.ServicesState)
}

// Check all the state transitions and only update HAproxy when a change
// will affect service availability.
func ShouldNotify(oldStatus int, newStatus int) bool {
	log.Debugf("Checking event. OldStatus: %s NewStatus: %s",
		service.StatusString(oldStatus), service.StatusString(newStatus),
	)

	// Compare old and new states to find significant changes only
	switch newStatus {
	case service.ALIVE:
		return true
	case service.TOMBSTONE:
		return true
	case service.UNKNOWN:
		if oldStatus == service.ALIVE {
			return true
		}
	case service.UNHEALTHY:
		if oldStatus == service.ALIVE {
			return true
		}
	default:
		log.Errorf("Got unknown service change status: %d", newStatus)
		return false
	}

	log.Debugf("Skipped HAproxy update due to state machine check")
	return false
}

// Used to fetch the current state from a Sidecar endpoint, usually
// on startup of this process, when the currentState is empty.
func FetchState(url string) (*catalog.ServicesState, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	state, err := catalog.Decode(bytes)
	if err != nil {
		return nil, err
	}

	return state, nil
}

// Loop forever, processing updates to the state.
func (rcvr *Receiver) ProcessUpdates() {
	for {
		// Batch up to RELOAD_BUFFER number updates into a
		// single update.
		first := <-rcvr.ReloadChan
		pending := len(rcvr.ReloadChan)

		// Call the callback
		if rcvr.OnUpdate == nil {
			log.Error("OnUpdate() callback not defined!")
		} else {
			rcvr.OnUpdate(rcvr.CurrentState)
		}

		// We just flushed the most recent state, dump all the
		// pending items up to that point.
		var reload time.Time
		for i := 0; i < pending; i++ {
			reload = <-rcvr.ReloadChan
		}

		if first.Before(reload) {
			log.Infof("Skipped %d messages between %s and %s", pending, first, reload)
		}

		// Don't notify more frequently than every RELOAD_HOLD_DOWN period. When a
		// deployment rolls across the cluster it can trigger a bunch of groupable
		// updates.
		log.Debug("Holding down...")
		time.Sleep(RELOAD_HOLD_DOWN)
	}
}

// Process and update by queueing a writeAndReload().
func (rcvr *Receiver) EnqueueUpdate() {
	rcvr.ReloadChan <- time.Now().UTC()
}

// On startup in standard mode, we need to bootstrap some state.
func (rcvr *Receiver) FetchInitialState(stateUrl string) error {
	rcvr.StateLock.Lock()
	defer rcvr.StateLock.Unlock()

	log.Info("Fetching initial state on startup...")
	state, err := FetchState(stateUrl)
	if err != nil {
		return err
	} else {
		log.Info("Successfully retrieved state")
		rcvr.CurrentState = state
		if rcvr.OnUpdate == nil {
			log.Error("OnUpdate() callback not defined!")
		} else {
			rcvr.OnUpdate(state)
		}
	}

	return nil
}
