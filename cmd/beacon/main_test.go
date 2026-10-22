package main

import (
	"reflect"
	"testing"

	"github.com/eemax/beacon/internal/feed"
)

func TestParseMetric(t *testing.T) {
	cases := map[string]feed.Metric{
		"n=3":                              {Key: "n", Value: 3},
		"duration=58:s":                    {Key: "duration", Value: 58, Unit: "s"},
		"records.new=12:count:New records": {Key: "records.new", Value: 12, Unit: "count", Label: "New records"},
		"records.new{endpoint=styles}=17:count:New": {Key: "records.new", Value: 17, Unit: "count", Label: "New", Labels: feed.Labels{"endpoint": "styles"}},
		"x{a=1,b=2}=0.5": {Key: "x", Value: 0.5, Labels: feed.Labels{"a": "1", "b": "2"}},
	}
	for in, want := range cases {
		got, err := parseMetric(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"n", "n=x", "n{a=1=3", "n{a}=1"} {
		if _, err := parseMetric(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseCheck(t *testing.T) {
	got, err := parseCheck("fresh{endpoint=styles}=warn:2 h old: retrying")
	want := feed.Check{Name: "fresh", Status: "warn", Detail: "2 h old: retrying", Labels: feed.Labels{"endpoint": "styles"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
}
