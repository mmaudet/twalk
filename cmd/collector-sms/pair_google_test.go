package main

import "testing"

func TestParseCookies(t *testing.T) {
	want := map[string]string{"SID": "s1", "HSID": "h1", "OSID": "o1", "SSID": "ss1", "APISID": "a1", "SAPISID": "sa1"}
	curl := `curl 'https://messages.google.com/web/config' -H 'accept: */*' -b '__Secure-1PSID=p1; SID=s1; __Secure-3PAPISID=x; HSID=h1; SSID=ss1; APISID=a1; SAPISID=sa1; OSID=o1; __Secure-1PSIDTS=ts1' -H 'user-agent: x'`
	header := "Cookie: SID=s1; HSID=h1; SSID=ss1; APISID=a1; SAPISID=sa1; OSID=o1"
	js := `{"SID":"s1","HSID":"h1","OSID":"o1","SSID":"ss1","APISID":"a1","SAPISID":"sa1","NID":"ignored"}`
	for name, in := range map[string]string{"curl": curl, "header": header, "json": js} {
		got, err := parseCookies(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
		if _, ok := got["NID"]; ok {
			t.Errorf("%s: unexpected cookie kept", name)
		}
	}
	if got, _ := parseCookies(curl); got["__Secure-1PSIDTS"] != "ts1" {
		t.Error("optional 1PSIDTS not kept")
	}
	if _, err := parseCookies("Cookie: __Secure-1PSID=p1; HSID=h1"); err == nil {
		t.Error("missing cookies accepted (SID must not match __Secure-1PSID)")
	}
}
