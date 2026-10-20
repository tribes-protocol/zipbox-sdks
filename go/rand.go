package zipbox

import "crypto/rand"

func cryptoRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("zipbox: crypto/rand failed: " + err.Error())
	}
}
