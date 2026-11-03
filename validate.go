package tavern

import (
	"errors"
	"fmt"
)

// optionSet records which composable options were passed.
type optionSet uint16

const (
	optScope optionSet = 1 << iota
	optFilter
	optRate
	optMeta
	optSnapshot
	optDisconnectOnLoss
	optTerminateOnInnerClose
	optTerminateOnReplayError
	optStatus
)

var optionNames = map[optionSet]string{
	optScope:                  "SubWithScope",
	optFilter:                 "SubWithFilter",
	optRate:                   "SubWithRate",
	optMeta:                   "SubWithMeta",
	optSnapshot:               "SubWithSnapshot",
	optDisconnectOnLoss:       "SubWithDisconnectOnLoss",
	optTerminateOnInnerClose:  "SubWithTerminateOnInnerClose",
	optTerminateOnReplayError: "SubWithTerminateOnReplayError",
	optStatus:                 "SubWithStatus",
}

func (o optionSet) String() string { return optionNames[o] }

// globUnsupported lists the options SubscribeGlobWith panics on, in the
// order it checks them.
var globUnsupported = []struct {
	opt optionSet
	set func(subscribeConfig) bool
}{
	{optDisconnectOnLoss, func(c subscribeConfig) bool { return c.disconnectOnLoss }},
	{optStatus, func(c subscribeConfig) bool { return c.status != nil }},
	{optTerminateOnReplayError, func(c subscribeConfig) bool { return c.terminateOnReplayError }},
}

// SubscribeTarget names the composable subscribe method that options are
// validated for.
type SubscribeTarget uint8

const (
	TargetSubscribeWith SubscribeTarget = iota + 1
	TargetSubscribeFromIDWith
	TargetSubscribeMultiWith
	TargetSubscribeGlobWith
)

var targetNames = [...]string{
	TargetSubscribeWith:       "SubscribeWith",
	TargetSubscribeFromIDWith: "SubscribeFromIDWith",
	TargetSubscribeMultiWith:  "SubscribeMultiWith",
	TargetSubscribeGlobWith:   "SubscribeGlobWith",
}

func (t SubscribeTarget) String() string {
	if t > 0 && int(t) < len(targetNames) {
		return targetNames[t]
	}
	return "unknown"
}

// Problems reported by [ValidateSubscribeOptions]. Each [OptionError] wraps
// one of them.
var (
	// ErrOptionConflict: options that the target rejects together; it panics.
	ErrOptionConflict = errors.New("conflicting options")
	// ErrOptionUnsupported: an option the target does not support; it panics.
	ErrOptionUnsupported = errors.New("unsupported option")
	// ErrOptionIgnored: an option or value that has no effect on the target.
	ErrOptionIgnored = errors.New("option has no effect")
	// ErrOptionInvalid: a nil option, an unknown target, or a status already
	// attached to a subscription. The subscribe method panics on all of
	// these except an unknown target.
	ErrOptionInvalid = errors.New("invalid option")
)

// OptionError describes one problem with a set of subscribe options. Err is
// one of ErrOptionConflict, ErrOptionUnsupported, ErrOptionIgnored or
// ErrOptionInvalid, so errors.Is works on it.
type OptionError struct {
	Target SubscribeTarget
	// Option is the option's constructor name, such as "SubWithRate".
	Option string
	Err    error
	Detail string
}

func (e *OptionError) Error() string {
	return fmt.Sprintf("tavern: %s: %s: %v: %s", e.Target, e.Option, e.Err, e.Detail)
}

func (e *OptionError) Unwrap() error { return e.Err }

// ValidateSubscribeOptions reports problems with opts for the given target
// without subscribing. It returns nil when the options are accepted and
// every option has an effect; otherwise an error joining one [OptionError]
// per problem (use errors.Is with the Err values, or errors.As to read
// details). Callers that accept no-op options can ignore errors matching
// only ErrOptionIgnored.
//
// Validation has no side effects: it does not call snapshot or filter
// functions, touch a broker, store or backend, or attach a
// [SubscriptionStatus], so it can run any number of times before the real
// subscribe call. It is advisory: it does not reserve the status or reserve
// admission, and a status validated as free can still be attached by
// another subscription before yours.
func ValidateSubscribeOptions(target SubscribeTarget, opts ...SubscribeOption) error {
	var errs []error
	reported := make(map[optionSet]error)
	add := func(opt optionSet, kind error, detail string) {
		if reported[opt] == kind {
			return
		}
		reported[opt] = kind
		errs = append(errs, &OptionError{Target: target, Option: opt.String(), Err: kind, Detail: detail})
	}
	if target.String() == "unknown" {
		return &OptionError{Target: target, Option: "-", Err: ErrOptionInvalid, Detail: "unknown subscribe target"}
	}

	var cfg subscribeConfig
	for i, opt := range opts {
		if opt == nil {
			errs = append(errs, &OptionError{Target: target, Option: fmt.Sprintf("option %d", i),
				Err: ErrOptionInvalid, Detail: "nil SubscribeOption"})
			continue
		}
		opt(&cfg)
	}
	given := cfg.given

	// Values that leave an option without effect.
	if given&optScope != 0 && cfg.scope == "" {
		add(optScope, ErrOptionIgnored, "an empty scope subscribes unscoped")
	}
	if given&optFilter != 0 && cfg.filter == nil {
		add(optFilter, ErrOptionIgnored, "a nil predicate filters nothing")
	}
	if given&optSnapshot != 0 && cfg.snapshotFn == nil {
		add(optSnapshot, ErrOptionIgnored, "a nil snapshot function sends no snapshot")
	}
	if given&optStatus != 0 && cfg.status == nil {
		add(optStatus, ErrOptionIgnored, "a nil status records nothing")
	}
	// With loss-awareness any rate is a conflict instead, except on globs,
	// which reject loss-awareness itself.
	if given&optRate != 0 && cfg.rate.interval() <= 0 && (!cfg.disconnectOnLoss || target == TargetSubscribeGlobWith) {
		add(optRate, ErrOptionIgnored, "a rate with no positive interval does not limit delivery")
	}
	if cfg.status != nil && cfg.status.isAttached() {
		add(optStatus, ErrOptionInvalid, "the status is already attached to a subscription; use a new one")
	}

	switch target {
	case TargetSubscribeGlobWith:
		for _, u := range globUnsupported {
			if u.set(cfg) {
				add(u.opt, ErrOptionUnsupported, "glob subscriptions do not support it")
			}
		}
		for _, opt := range []optionSet{optMeta, optSnapshot, optTerminateOnInnerClose} {
			if given&opt != 0 {
				add(opt, ErrOptionIgnored, "glob subscriptions do not use it")
			}
		}
	default:
		if cfg.lossWithRate() {
			add(optRate, ErrOptionConflict, "SubWithDisconnectOnLoss cannot be combined with SubWithRate")
		}
		if target != TargetSubscribeMultiWith && given&optTerminateOnInnerClose != 0 {
			add(optTerminateOnInnerClose, ErrOptionIgnored, "it only applies to SubscribeMultiWith")
		}
		// SubscribeWith (and each topic of SubscribeMultiWith) reads the
		// ReplayStore only without a filter or snapshot, unless the
		// subscription is loss-aware.
		if target != TargetSubscribeFromIDWith && cfg.terminateOnReplayError && !cfg.disconnectOnLoss &&
			(cfg.filter != nil || cfg.snapshotFn != nil) {
			add(optTerminateOnReplayError, ErrOptionIgnored,
				"filtered and snapshot subscriptions do not read the ReplayStore")
		}
	}
	return errors.Join(errs...)
}
