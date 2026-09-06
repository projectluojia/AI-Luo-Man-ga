package qq

import "context"

type testScheduler struct{}

func (testScheduler) Enqueue(context.Context, string) {}
