package sfu

// import (
// 	"log"
// 	"testing"

// 	"github.com/stretchr/testify/require"
// )

// // 65526, 65527, 65527, 65526, 65532, 65531, 65533, 65534, 65535, 1, 2, 3, 4, 5, 65528, 65529, 65530, 0, 6, 7, 8, 9, 10
// // 65526 1 1
// // 65527 1 1
// // 65528 1 1
// // 65529 1 1
// // 65530 1 1
// // 65531 2 1 ==> updscale ( don't upscale because the next packet already sent)
// // 65532 2 1
// // 65533 2 1
// // 65534 2 1
// // 65535 2 1
// // 0 2 1
// // 1 2 1
// // 2 2 1
// // 3 2 1
// // 4 2 1
// // 5 3 3 upscale
// // 6 3 3
// // 7 3 3
// // 8 3 3
// // 9 3 3
// // 10 3 3
// func TestAddCache(t *testing.T) {
// 	t.Parallel()

// 	caches := newPacketCaches()

// 	currentTID := uint8(1)
// 	currentSID := uint8(1)

// 	var sid, tid uint8
// 	var err error

// 	for i, unsorted := range unsortedNumbers {
// 		if unsorted < 65531 && unsorted > 65525 {
// 			if unsorted == 65526 || unsorted == 65527 {
// 				_, tid, sid, err = caches.GetDecision(unsorted, 65526, currentSID, currentTID)
// 				if i > 1 {
// 					require.Equal(t, err, ErrDuplicate)
// 				}
// 			} else if unsorted == 65531 {
// 				_, tid, sid, err = caches.GetDecision(unsorted, 65526, currentSID, currentTID)
// 				require.Equal(t, err, ErrCantDecide)
// 			} else {
// 				_, tid, sid, err = caches.GetDecision(unsorted, 65526, currentSID, currentTID)
// 				require.NoError(t, err, "Error should be nil for sequence number %d", unsorted)
// 			}

// 			if unsorted > 65526 {
// 				require.Equal(t, uint8(1), tid, "TID should be 3 for sequence number %d", unsorted, tid)
// 				require.Equal(t, uint8(1), sid, "SID should be 3 for sequence number %d", unsorted, sid)
// 			}
// 			caches.Add(unsorted, 65526, uint8(1), uint8(1))
// 		} else if unsorted > 4 && unsorted < 11 {
// 			if unsorted == 5 {
// 				_, tid, sid, err = caches.GetDecision(unsorted, 65526, currentSID, currentTID)
// 			} else {
// 				_, tid, sid, err = caches.GetDecision(unsorted, 65526, currentSID, currentTID)
// 			}
// 			require.NoError(t, err)
// 			if unsorted > 5 {
// 				require.Equal(t, uint8(3), tid, "TID should be 3 for sequence number %d", unsorted, tid)
// 				require.Equal(t, uint8(3), sid, "SID should be 3 for sequence number %d", unsorted, sid)
// 			}
// 			caches.Add(unsorted, 65526, uint8(3), uint8(3))
// 		} else {

// 			if unsorted > 65530 || unsorted < 5 {
// 				require.Equal(t, uint8(1), tid, "TID should be 3 for sequence number %d", unsorted, tid)
// 				require.Equal(t, uint8(1), sid, "SID should be 3 for sequence number %d", unsorted, sid)
// 			}

// 			caches.Add(unsorted, uint8(1), uint8(1))
// 		}
// 	}

// 	i := 0
// 	for e := caches.caches.Front(); e != nil; e = e.Next() {
// 		cache := e.Value.(Cache)
// 		require.Equal(t, sortedNumbers[i], cache.SeqNum)
// 		log.Println("Cache: ", cache.SeqNum, cache.TID, cache.SID)
// 		i++
// 	}
// }
