package main

import (
	"net"
	"testing"
)

func TestTestBuildExposureCheck(t *testing.T) {
	loop4 := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8787}
	loop6 := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8787}
	anyAddr := &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8787}
	any6 := &net.TCPAddr{IP: net.IPv6unspecified, Port: 8787}
	lan := &net.TCPAddr{IP: net.ParseIP("192.168.1.10"), Port: 8787}
	tailnet := &net.TCPAddr{IP: net.ParseIP("100.64.0.9"), Port: 443}

	for name, tc := range map[string]struct {
		dev, allow bool
		addr       net.Addr
		wantErr    bool
	}{
		"release build anywhere":     {false, false, anyAddr, false},
		"release build on tailnet":   {false, false, nil, false},
		"dev build on 127.0.0.1":     {true, false, loop4, false},
		"dev build on ::1":           {true, false, loop6, false},
		"dev build on 0.0.0.0":       {true, false, anyAddr, true},
		"dev build on [::]":          {true, false, any6, true},
		"dev build on LAN address":   {true, false, lan, true},
		"dev build on tailnet IP":    {true, false, tailnet, true},
		"dev build on tsnet (no IP)": {true, false, nil, true},
		"dev build, opted in":        {true, true, anyAddr, false},
		"dev build tsnet, opted in":  {true, true, nil, false},
	} {
		err := checkTestBuildExposure(tc.dev, tc.addr, tc.allow)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", name, err, tc.wantErr)
		}
	}
}
