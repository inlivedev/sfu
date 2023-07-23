package sfu

import (
	"bufio"
	"math/rand"
	"strings"

	"github.com/speps/go-hashids"
)

func GetUfragAndPass(sdp string) (ufrag, pass string) {
	scanner := bufio.NewScanner(strings.NewReader(sdp))
	iceUfrag := "a=ice-ufrag:"
	icePwd := "a=ice-pwd:" //nolint:gosec //it's not a password

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, iceUfrag) {
			ufrag = strings.Replace(line, iceUfrag, "", 1)
		} else if strings.Contains(line, icePwd) {
			pass = strings.Replace(line, icePwd, "", 1)
		}

		if ufrag != "" && pass != "" {
			break
		}
	}

	return ufrag, pass
}

func CountTracks(sdp string) int {
	counter := 0

	scanner := bufio.NewScanner(strings.NewReader(sdp))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "m=audio") || strings.Contains(line, "m=video") {
			counter++
		}
	}

	return counter
}

func GenerateID(data []int) string {
	randInt := rand.Intn(100) //nolint:gosec //it's not a password
	data = append(data, randInt)
	hd := hashids.NewData()
	hd.Salt = "this is my salt"
	hd.MinLength = 9
	h, _ := hashids.NewWithData(hd)
	e, _ := h.Encode(data)

	return e
}
