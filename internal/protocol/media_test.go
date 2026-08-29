package protocol

import "testing"

func TestParseImageAndVideoSSEEvents(t *testing.T) {
	image := ParseMediaData([]byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"/images/a.png","progress":100}}}}`))
	if len(image) != 1 || image[0].Kind != "image" || image[0].Progress != 100 {
		t.Fatalf("unexpected image events: %#v", image)
	}
	video := ParseMediaData([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"videoUrl":"/v.mp4","progress":100,"assetId":"v1"}}}}`))
	if len(video) != 1 || video[0].Kind != "video" || video[0].AssetID != "v1" {
		t.Fatalf("unexpected video events: %#v", video)
	}
}

func TestParseToolCalls(t *testing.T) {
	text := `<tool_calls><tool_call><tool_name>search</tool_name><parameters>{"query":"go"}</parameters></tool_call></tool_calls>`
	calls := ParseToolCalls(text, []string{"search"})
	if len(calls) != 1 || calls[0].Name != "search" || calls[0].Arguments != `{"query":"go"}` {
		t.Fatalf("unexpected tool calls: %#v", calls)
	}
}

func TestParseDataURI(t *testing.T) {
	filename, mime, encoded, err := ParseDataURI("data:image/png;base64,aGk=")
	if err != nil || filename != "file.png" || mime != "image/png" || encoded != "aGk=" {
		t.Fatalf("unexpected data URI result: %q %q %q %v", filename, mime, encoded, err)
	}
}

func TestVideoSegmentLengths(t *testing.T) {
	want := map[int][]int{6: {6}, 10: {10}, 12: {6, 6}, 16: {10, 6}, 20: {10, 10}}
	for seconds, expected := range want {
		got, ok := VideoSegmentLengths(seconds)
		if !ok || len(got) != len(expected) {
			t.Fatalf("unexpected segments for %d: %#v %v", seconds, got, ok)
		}
		for index := range expected {
			if got[index] != expected[index] {
				t.Fatalf("unexpected segments for %d: %#v", seconds, got)
			}
		}
	}
	if _, ok := VideoSegmentLengths(7); ok {
		t.Fatal("unsupported video length accepted")
	}
}
