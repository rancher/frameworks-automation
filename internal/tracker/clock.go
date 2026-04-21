package tracker

import "time"

// nowFn is the clock used by Render-via-renderForCreate. Overridden in tests.
var nowFn = func() time.Time { return time.Now() }
