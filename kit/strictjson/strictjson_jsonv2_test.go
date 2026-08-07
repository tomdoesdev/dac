//go:build goexperiment.jsonv2

package strictjson

import (
	jsonv2 "encoding/json/v2"
	"testing"
)

// TestJSONV2Agreement compares the public defaults against the standard
// library implementation that will eventually replace this package's scanner.
func TestJSONV2Agreement(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"name":"a","count":2}`),
		[]byte(`{"name":"a","extra":1}`),
		[]byte(`{"Name":"a"}`),
		[]byte(`{"name":"a","name":"b"}`),
		[]byte(`{"inner":{"x":1,"x":2}}`),
		[]byte(`{"name":"a"} {"name":"b"}`),
		[]byte(`{"inner":{"x":[1,2,{"z":3}]}}`),
		[]byte(`{"name":`),
	}
	for _, data := range cases {
		t.Run(string(data), func(t *testing.T) {
			var strict document
			strictErr := Unmarshal(data, &strict)
			var standard document
			standardErr := jsonv2.Unmarshal(data, &standard, jsonv2.RejectUnknownMembers(true))
			if (strictErr == nil) != (standardErr == nil) {
				t.Fatalf("acceptance differs: strictjson=%v, json/v2=%v", strictErr, standardErr)
			}
		})
	}
}
