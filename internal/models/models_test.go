package models

import "testing"

func TestParseFixedPoint(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "typical price", input: "0.0800", want: 80_000},
		{name: "three decimal places", input: "0.960", want: 960_000},
		{name: "quantity with two decimals", input: "300.00", want: 300_000_000},
		{name: "negative delta", input: "-54.00", want: -54_000_000},
		{name: "whole number, no decimal point", input: "5", want: 5_000_000},
		{name: "leading plus sign", input: "+5.5", want: 5_500_000},
		{name: "zero", input: "0", want: 0},
		{name: "six decimal digits, exactly at precision limit", input: "0.123456", want: 123_456},
		{name: "empty string is an error", input: "", wantErr: true},
		{name: "whitespace only is an error", input: "   ", wantErr: true},
		{name: "too many decimal digits loses precision, is an error", input: "0.1234567", wantErr: true},
		{name: "non-numeric whole part is an error", input: "abc.00", wantErr: true},
		{name: "non-numeric fractional part is an error", input: "0.ab", wantErr: true},
		{name: "lone minus sign is an error", input: "-", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFixedPoint(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseFixedPoint(%q) = %v, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFixedPoint(%q) returned unexpected error: %v", tc.input, err)
			}
			if got.Micros != tc.want {
				t.Errorf("ParseFixedPoint(%q).Micros = %d, want %d", tc.input, got.Micros, tc.want)
			}
		})
	}
}

func TestFixedPointDollars(t *testing.T) {
	fp := FixedPoint{Micros: 650_000}
	if got, want := fp.Dollars(), 0.65; got != want {
		t.Errorf("Dollars() = %v, want %v", got, want)
	}
}
