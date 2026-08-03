package application

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// parallel runs one operation for each asset coordinate, at most limit at a
// time, and returns the results in coordinate order. The first failure cancels
// the rest.
//
// An operation whose context is already cancelled belongs to that first
// failure, not to a problem of its own. Callers check ctx.Err() before they
// report anything, because errgroup cancels with the first error as the
// context cause: an asset cut short mid-transfer would otherwise surface a
// message describing a completely different asset's failure.
func parallel[K fmt.Stringer, T any](ctx context.Context, limit int, names []K, operation func(context.Context, K) (T, error)) ([]T, error) {
	group, runCtx := errgroup.WithContext(ctx)
	group.SetLimit(max(limit, 1))
	results := make([]T, len(names))
	for index, name := range names {
		group.Go(func() error {
			// Another asset has already failed and cancelled the run. Report
			// nothing for this one: a column of cancellations would bury the
			// single failure that caused them.
			if runCtx.Err() != nil {
				return nil
			}
			value, err := operation(runCtx, name)
			if err != nil {
				return withAsset(err, name.String())
			}
			results[index] = value
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, networkError(err)
	}
	return results, nil
}
