package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/log"
	"golang.org/x/net/context"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/types"
)

const ResolveTimeout = 30 * time.Second

// Dispatcher sorts incoming alerts into aggregation groups and
// assigns the correct notifiers to each.
type Dispatcher struct {
	routes Routes
	alerts provider.Alerts

	aggrGroups map[model.Fingerprint]*aggrGroup
	notifiers  map[string]Notifier

	mtx    sync.RWMutex
	done   chan struct{}
	ctx    context.Context
	cancel func()
}

// NewDispatcher returns a new Dispatcher.
func NewDispatcher(ap provider.Alerts) *Dispatcher {
	return &Dispatcher{alerts: ap}
}

// ApplyConfig updates the dispatcher to match the new configuration.
func (d *Dispatcher) ApplyConfig(conf *config.Config) {
	d.mtx.Lock()
	defer d.mtx.Unlock()

	// If a cancelation function is set, the dispatcher is running.
	if d.cancel != nil {
		d.Stop()
		defer func() { go d.Run() }()
	}

	d.routes = NewRoutes(conf.Routes)
	d.notifiers = map[string]Notifier{}

	// TODO(fabxc): build correct notifiers from new conf.NotificationConfigs.
	for _, ncfg := range conf.NotificationConfigs {
		d.notifiers[ncfg.Name] = &LogNotifier{ncfg.Name}
	}
}

// Run starts dispatching alerts incoming via the updates channel.
func (d *Dispatcher) Run() {
	d.done = make(chan struct{})
	d.aggrGroups = map[model.Fingerprint]*aggrGroup{}

	d.ctx, d.cancel = context.WithCancel(context.Background())

	updates := d.alerts.IterActive()
	defer updates.Close()

	d.run(updates.Next())
}

func (d *Dispatcher) run(updates <-chan *types.Alert) {
	cleanup := time.NewTicker(15 * time.Second)
	defer cleanup.Stop()

	for {
		select {
		case alert := <-updates:
			d.mtx.RLock()
			routes := d.routes.Match(alert.Labels)
			d.mtx.RUnlock()

			for _, r := range routes {
				d.processAlert(alert, r)
			}

		case <-cleanup.C:
			for _, ag := range d.aggrGroups {
				if ag.empty() {
					ag.stop()
					delete(d.aggrGroups, ag.fingerprint())
				}
			}

		case <-d.ctx.Done():
			return
		}
	}
}

// Stop the dispatcher.
func (d *Dispatcher) Stop() {
	d.cancel()
	d.cancel = nil

	<-d.done
}

// notifyFunc is a function that performs notifcation for the alert
// with the given fingerprint. It aborts on context cancelation.
// Returns false iff notifying failed.
type notifyFunc func(context.Context, ...*types.Alert) bool

// notifyFunc returns a function which performs a notification
// as required by the routing options.
func (d *Dispatcher) notifyFunc(dest string) notifyFunc {
	d.mtx.Lock()
	defer d.mtx.Unlock()

	notifier := d.notifiers[dest]

	return func(ctx context.Context, alerts ...*types.Alert) bool {
		if err := notifier.Notify(ctx, alerts...); err != nil {
			log.Errorf("Notify for %d alerts failed: %s", len(alerts), err)
			return false
		}
		return true
	}
}

// processAlert determins in which aggregation group the alert falls
// and insert it.
func (d *Dispatcher) processAlert(alert *types.Alert, opts *RouteOpts) {
	group := model.LabelSet{}

	for ln, lv := range alert.Labels {
		if _, ok := opts.GroupBy[ln]; ok {
			group[ln] = lv
		}
	}

	fp := group.Fingerprint()

	// If the group does not exist, create it.
	ag, ok := d.aggrGroups[fp]
	if !ok {
		ag = newAggrGroup(d.ctx, group, opts)
		go ag.run(d.notifyFunc(opts.SendTo))

		d.aggrGroups[fp] = ag
	}

	ag.insert(alert)
}

// aggrGroup aggregates alert fingerprints into groups to which a
// common set of routing options applies.
// It emits notifications in the specified intervals.
type aggrGroup struct {
	labels model.LabelSet
	opts   *RouteOpts

	ctx    context.Context
	cancel func()
	done   chan struct{}
	next   *time.Timer

	mtx     sync.RWMutex
	alerts  map[model.Fingerprint]*types.Alert
	hasSent bool
}

// newAggrGroup returns a new aggregation group.
func newAggrGroup(ctx context.Context, labels model.LabelSet, opts *RouteOpts) *aggrGroup {
	ag := &aggrGroup{
		labels: labels,
		opts:   opts,
		alerts: map[model.Fingerprint]*types.Alert{},
	}
	ag.ctx, ag.cancel = context.WithCancel(ctx)

	return ag
}

func (ag *aggrGroup) String() string {
	return fmt.Sprintf("%x", ag.fingerprint())
}

func (ag *aggrGroup) run(notify notifyFunc) {
	ag.done = make(chan struct{})

	// Set an initial one-time wait before flushing
	// the first batch of notifications.
	ag.next = time.NewTimer(ag.opts.GroupWait)

	defer close(ag.done)
	defer ag.next.Stop()

	for {
		select {
		case <-ag.next.C:
			// Give the notifcations time until the next flush to
			// finish before terminating them.
			ctx, _ := context.WithTimeout(ag.ctx, ag.opts.GroupInterval)

			// Wait the configured interval before calling flush again.
			ag.next.Reset(ag.opts.GroupInterval)

			ag.flush(func(alerts ...*types.Alert) bool {
				return notify(ctx, alerts...)
			})

		case <-ag.ctx.Done():
			return
		}
	}
}

func (ag *aggrGroup) stop() {
	// Calling cancel will terminate all in-process notifications
	// and the run() loop.
	ag.cancel()
	<-ag.done
}

func (ag *aggrGroup) fingerprint() model.Fingerprint {
	return ag.labels.Fingerprint()
}

// insert the alert into the aggregation group. If the aggregation group
// is empty afterwards, true is returned.
func (ag *aggrGroup) insert(alert *types.Alert) {
	ag.mtx.Lock()
	defer ag.mtx.Unlock()

	ag.alerts[alert.Fingerprint()] = alert

	// Immediately trigger a flush if the wait duration for this
	// alert is already over.
	if !ag.hasSent && alert.Timestamp.Add(ag.opts.GroupWait).Before(time.Now()) {
		ag.next.Reset(0)
	}
}

func (ag *aggrGroup) empty() bool {
	ag.mtx.RLock()
	defer ag.mtx.RUnlock()

	return len(ag.alerts) == 0
}

// flush sends notifications for all new alerts.
func (ag *aggrGroup) flush(notify func(...*types.Alert) bool) {
	ag.mtx.Lock()

	var (
		alerts      = make(map[model.Fingerprint]*types.Alert, len(ag.alerts))
		alertsSlice = make([]*types.Alert, 0, len(ag.alerts))
	)
	for fp, alert := range ag.alerts {
		alerts[fp] = alert
		alertsSlice = append(alertsSlice, alert)
	}

	ag.mtx.Unlock()

	if notify(alertsSlice...) {
		ag.mtx.Lock()
		for fp, a := range alerts {
			// Only delete if the fingerprint has not been inserted
			// again since we notified about it.
			if a.Resolved() && ag.alerts[fp] == a {
				delete(ag.alerts, fp)
			}
		}
		ag.mtx.Unlock()
	}

	ag.hasSent = true
}