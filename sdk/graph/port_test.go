package graph

import "testing"

func TestIsCompatible(t *testing.T) {
	tests := []struct {
		out, in  PortType
		expected bool
	}{
		{PortTypeString, PortTypeString, true},
		{PortTypeString, PortTypeAny, true},
		{PortTypeAny, PortTypeString, true},
		{PortTypeAny, PortTypeAny, true},
		{PortTypeInteger, PortTypeFloat, true},
		{PortTypeFloat, PortTypeInteger, false},
		{PortTypeString, PortTypeInteger, false},
		{PortTypeMessages, PortTypeMessages, true},
		{PortTypeMessages, PortTypeString, false},
		{PortTypeUsage, PortTypeAny, true},
	}

	for _, tt := range tests {
		result := IsCompatible(tt.out, tt.in)
		if result != tt.expected {
			t.Errorf("IsCompatible(%s, %s) = %v, want %v", tt.out, tt.in, result, tt.expected)
		}
	}
}
