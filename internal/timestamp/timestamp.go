package timestamp

import "time"

const Format = "2006-01-02T15:04:05.000Z"

func Now() string {
	return time.Now().UTC().Format(Format)
}
