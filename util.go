package sfu

import (
	"bufio"
	"strings"
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
