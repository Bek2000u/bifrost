package schemas

import "testing"

func TestSpeechBackfillPreservesBilledCharacters(t *testing.T) {
	request := &BifrostSpeechRequest{Input: &SpeechInput{Input: "Привет"}}
	for _, billed := range []int{0, 19} {
		want := billed
		if want == 0 {
			want = 6
		}
		response := &BifrostSpeechResponse{Usage: &SpeechUsage{InputChars: billed, AudioSeconds: 7, Cost: &BifrostCost{TotalCost: 0.12}}}
		response.BackfillParams(request)
		if response.Usage.InputChars != want || response.Usage.AudioSeconds != 7 || response.Usage.Cost.TotalCost != 0.12 {
			t.Fatalf("provider usage overwritten: %+v, want chars %d", response.Usage, want)
		}
		stream := &BifrostSpeechStreamResponse{Usage: &SpeechUsage{InputChars: billed}}
		stream.BackfillParams(request)
		if stream.Usage.InputChars != want {
			t.Fatalf("stream chars = %d, want %d", stream.Usage.InputChars, want)
		}
	}
	response := &BifrostSpeechResponse{}
	response.BackfillParams(request)
	if response.Usage == nil || response.Usage.InputChars != 6 {
		t.Fatal("Unicode character count was not backfilled")
	}
}

func BenchmarkSpeechBackfillBilledCharacters(b *testing.B) {
	request := &BifrostSpeechRequest{Input: &SpeechInput{Input: "Привет, мир"}}
	response := &BifrostSpeechResponse{Usage: &SpeechUsage{InputChars: 19}}
	b.ReportAllocs()
	for b.Loop() {
		response.BackfillParams(request)
	}
}
