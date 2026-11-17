package main

import (
	"errors"
	"fmt"
	"strings"
)

const joinCodePrefix = "ohya1"

func formatJoinCode(invitation, hubAddr string) string {
	return joinCodePrefix + "." + invitation + "." + hubAddr
}

func parseJoinCode(code string) (invitation, hubAddr string, err error) {
	rest, ok := strings.CutPrefix(code, joinCodePrefix+".")
	if !ok {
		if strings.Contains(code, ":") {
			return "", "", errors.New(
				"this join code uses the retired <invitation>:<preauth-key> format; ask the hub for an ohya1.<invitation>.<address> code from ohya devices invite")
		}
		return "", "", fmt.Errorf(
			"a join code starts with %s.; get one from ohya devices invite on the hub", joinCodePrefix+".")
	}
	invitation, hubAddr, ok = strings.Cut(rest, ".")
	switch {
	case !ok || invitation == "" || hubAddr == "":
		return "", "", fmt.Errorf(
			"a join code is %s.<invitation>.<tailcat-address>; get one from ohya devices invite on the hub", joinCodePrefix)
	case !strings.HasPrefix(hubAddr, "tc") || strings.ContainsAny(hubAddr, ".:"):
		return "", "", errors.New("the hub address in this join code is not a tailcat address; get a fresh code from ohya devices invite")
	case len(invitation) > 128 || len(hubAddr) > 8192:
		return "", "", errors.New("this join code is not an ohya1 code; get one from ohya devices invite on the hub")
	}
	return invitation, hubAddr, nil
}
