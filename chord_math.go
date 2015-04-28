package dendrite

import (
	"bytes"
	//"log"
	"crypto/sha1"
	"math/big"
	"math/rand"
	"time"
)

func min(a, b int) int {
	if a <= b {
		return a
	} else {
		return b
	}
}
func Min(a, b int) int {
	return min(a, b)
}

// Generates a random stabilization time
func randStabilize(conf *Config) time.Duration {
	min := conf.StabilizeMin
	max := conf.StabilizeMax
	rand.Seed(time.Now().UnixNano())
	r := rand.Float64()
	return time.Duration((r * float64(max-min)) + float64(min))
}

// Checks if a key is between two keys (id1 > key > id2)
// rincl if true, adds variation (id1 > key >= id2)
func between(id1, id2, key []byte, rincl bool) bool {
	// Check for ring wrap around
	if bytes.Compare(id1, id2) == 1 {
		if rincl {
			return bytes.Compare(id1, key) == -1 ||
				bytes.Compare(id2, key) >= 0
		}
		return bytes.Compare(id1, key) == -1 ||
			bytes.Compare(id2, key) == 1
	}

	// Handle the normal case
	if rincl {
		return bytes.Compare(id1, key) == -1 &&
			bytes.Compare(id2, key) >= 0
	}
	return bytes.Compare(id1, key) == -1 &&
		bytes.Compare(id2, key) == 1
}

func Between(id1, id2, key []byte, rincl bool) bool {
	return between(id1, id2, key, rincl)
}

// Returns the vnode nearest to a key
func nearestVnodeToKey(vnodes []*localVnode, key []byte) *Vnode {
	for i := len(vnodes) - 1; i >= 0; i-- {
		if bytes.Compare(vnodes[i].Id, key) == -1 {
			return &vnodes[i].Vnode
		}
	}
	// Return the last vnode
	return &vnodes[len(vnodes)-1].Vnode
}

// Computes the offset by (n + 2^exp) % (2^mod)
func powerOffset(id []byte, exp int, mod int) []byte {
	// Copy the existing slice
	off := make([]byte, len(id))
	copy(off, id)

	// Convert the ID to a bigint
	idInt := big.Int{}
	idInt.SetBytes(id)

	// Get the offset
	two := big.NewInt(2)
	offset := big.Int{}
	offset.Exp(two, big.NewInt(int64(exp)), nil)

	// Sum
	sum := big.Int{}
	sum.Add(&idInt, &offset)

	// Get the ceiling
	ceil := big.Int{}
	ceil.Exp(two, big.NewInt(int64(mod)), nil)

	// Apply the mod
	idInt.Mod(&sum, &ceil)

	// Add together
	return idInt.Bytes()
}

func distance(a, b []byte) *big.Int {
	// Get the ring size
	var ring big.Int
	ring.Exp(big.NewInt(2), big.NewInt(int64(160)), nil)
	// Convert to int
	var a_int, b_int, dist big.Int
	(&a_int).SetBytes(a)
	(&b_int).SetBytes(b)
	(&dist).SetInt64(0)

	cmp := bytes.Compare(a, b)
	switch cmp {
	case 0:
		return &dist
	case -1:
		return (&dist).Sub(&b_int, &a_int)
	default:
		// loop the ring
		(&dist).Sub(&ring, &a_int)
		return (&dist).Add(&dist, &b_int)
	}

}

func HashKey(key []byte) []byte {
	hash := sha1.New()
	hash.Write(key)
	return hash.Sum(nil)
}
