package scenarios

import (
	"math"
	"testing"
)

func TestScoreEMF1(t *testing.T) {
	em, f1 := scoreEMF1("She lives in San Francisco now.", []string{"San Francisco"})
	if em != 1 {
		t.Fatalf("em = %v", em)
	}
	if f1 <= 0 {
		t.Fatalf("f1 = %v", f1)
	}
	em, f1 = scoreEMF1("unknown", []string{"San Francisco"})
	if em != 0 || f1 != 0 {
		t.Fatalf("miss: em=%v f1=%v", em, f1)
	}
}

func TestScoreItemRecall(t *testing.T) {
	tests := []struct {
		name       string
		prediction string
		golds      []string
		want       float64
	}{
		{
			name:       "all items",
			prediction: "pottery classes, camping, painting and swimming",
			golds:      []string{"pottery, camping, painting, swimming"},
			want:       1,
		},
		{
			name:       "partial items",
			prediction: "Melanie does pottery and enjoys hiking.",
			golds:      []string{"pottery, camping, painting, swimming"},
			want:       0.25,
		},
		{
			name:       "none",
			prediction: "I don't know",
			golds:      []string{"pottery, camping, painting, swimming"},
			want:       0,
		},
		{
			name:       "single concrete fact contained",
			prediction: "She moved from Sweden 4 years ago.",
			golds:      []string{"Sweden"},
			want:       1,
		},
		{
			name:       "single concrete fact missed",
			prediction: "She moved from her home country.",
			golds:      []string{"Sweden"},
			want:       0,
		},
		{
			name:       "no answer matches nothing",
			prediction: "I don't know",
			golds:      []string{"No"},
			want:       0,
		},
		{
			name:       "best gold answer wins",
			prediction: "the military aptitude test",
			golds:      []string{"the military aptitude test", "the aptitude exam"},
			want:       1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := scoreItemRecall(test.prediction, test.golds)
			if math.Abs(got-test.want) > 1e-9 {
				t.Fatalf("scoreItemRecall(%q, %v) = %v, want %v", test.prediction, test.golds, got, test.want)
			}
		})
	}
}
