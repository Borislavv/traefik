package model

type Releaser func(queryHeaders *[][2][]byte, responseHeaders *[][2][]byte)

var emptyReleaser Releaser = func(_, _ *[][2][]byte) {}
