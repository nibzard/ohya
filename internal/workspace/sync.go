package workspace

import "context"

const MaxRounds = 64

type Exchange func(ctx context.Context, observed []Observation) (pending []string, err error)

type Resolver func(path string) (Observation, error)

type SyncResult struct {
	Observed int
	Failures []error
}

func Sync(ctx context.Context, exchange Exchange, resolve Resolver) (SyncResult, error) {
	var res SyncResult
	tried := map[string]bool{}
	var observed []Observation
	for round := 0; round < MaxRounds; round++ {
		pending, err := exchange(ctx, observed)
		if err != nil {
			return res, err
		}
		res.Observed += len(observed)
		observed = nil
		for _, path := range pending {
			if tried[path] {
				continue
			}
			tried[path] = true
			o, err := resolve(path)
			if err != nil {
				res.Failures = append(res.Failures, err)
				continue
			}
			observed = append(observed, o)
		}
		if len(observed) == 0 {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	if _, err := exchange(ctx, observed); err != nil {
		return res, err
	}
	res.Observed += len(observed)
	return res, nil
}
