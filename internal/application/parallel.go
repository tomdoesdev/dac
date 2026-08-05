package application

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// parallel runs one operation for each asset coordinate, at most limit at a time, and returns the results in coordinate order.
// An operation whose context is already cancelled belongs to that first failure, not to a problem of its own.
func parallel[K fmt.Stringer, T any](ctx context.Context, limit int, names []K, operation func(context.Context, K) (T, error)) ([]T, error) {
	group, runCtx := errgroup.WithContext(ctx)
	group.SetLimit(max(limit, 1))
	results := make([]T, len(names))
	for index, name := range names {
		group.Go(func() error {
			// Another asset has already failed and cancelled the run.
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
