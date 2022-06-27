package v1

import (
	"github.com/bloxapp/ssv/network/forks"
	scrypto "github.com/bloxapp/ssv/utils/crypto"
)

// MsgID returns msg_id for the given message
func (v1 *ForkV1) MsgID() forks.MsgIDFunc {
	return func(msg []byte) string {
		if len(msg) == 0 {
			return ""
		}
		// TODO: check performance
		h := scrypto.Sha256Hash(msg)
		return string(h[20:])
	}
}

// Subnets returns the subnets count for this fork
func (v1 *ForkV1) Subnets() int64 {
	return int64(SubnetsCount)
}