package retry // import "v2ray.com/core/common/retry"

//go:generate errorgen

import (
	"github.com/go-errors/errors"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"time"
)

// Strategy is a way to retry on a specific function.
type Strategy interface {
	// On performs a retry on a specific function, until it doesn't return any error.
	On(func() error) error
}

type retryer struct {
	totalAttempt int
	nextDelay    func() uint32
}

// On implements Strategy.On.
func (r *retryer) On(method func() error) error {
	attempt := 0
	accumulatedError := make([]error, 0, r.totalAttempt)
	for attempt < r.totalAttempt {
		err := method()
		if err == nil {
			if attempt > 0 {
				log.Infof(" Exec success after  %d times retry.DoubleTimed", attempt)
			}
			return nil
		}
		numErrors := len(accumulatedError)
		if numErrors == 0 || err.Error() != accumulatedError[numErrors-1].Error() {
			accumulatedError = append(accumulatedError, err)
		}
		delay := r.nextDelay()
		time.Sleep(time.Duration(delay) * time.Millisecond)
		attempt++
	}
	return errors.New("全部失败")
}

// Timed returns a retry strategy with fixed interval.
func Timed(attempts int, delay uint32) Strategy {
	return &retryer{
		totalAttempt: attempts,
		nextDelay: func() uint32 {
			return delay
		},
	}
}
func DoubleTimed(attempts int, MaxDelay uint32) Strategy {
	nextDelay := uint32(0)
	nextAtt := uint32(1)
	return &retryer{
		totalAttempt: attempts,
		nextDelay: func() uint32 {
			r := nextDelay
			if r > MaxDelay {
				r = MaxDelay
			} else {
				nextDelay += 97 + nextAtt*97
				nextAtt++
			}
			return r
		},
	}
}
func ExponentialBackoff(attempts int, delay uint32) Strategy {
	nextDelay := uint32(0)
	return &retryer{
		totalAttempt: attempts,
		nextDelay: func() uint32 {
			r := nextDelay
			nextDelay += delay
			return r
		},
	}
}
