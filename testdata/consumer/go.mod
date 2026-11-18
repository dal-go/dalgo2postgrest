module github.com/dal-go/dalgo2postgrest-acceptance

go 1.27.0

// This module is a consumer, not a working checkout. It deliberately pins the
// published module that the reviewed DataTug CLI ships, with no replace
// directive: the acceptance must exercise exactly that public artifact, not
// this repository's working tree.
require (
	github.com/dal-go/dalgo v0.95.4
	github.com/dal-go/dalgo2postgrest v0.0.0-20261027200821-d952c8fc3279
	github.com/dal-go/record v0.1.5
)

require (
	github.com/RoaringBitmap/roaring/v2 v2.30.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/strongo/random v0.0.3 // indirect
	golang.org/x/sys v0.30.0 // indirect
)
