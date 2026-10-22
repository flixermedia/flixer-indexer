package probe

import "testing"

func TestParseVideo(t *testing.T) {
	res, err := Parse([]byte(`{
	  "format": {"format_name": "matroska,webm", "duration": "8880.5", "bit_rate": "52000000", "tags": {"TITLE": "Movie"}},
	  "streams": [
	    {"codec_type":"video","codec_name":"hevc","width":3840,"height":2160,"avg_frame_rate":"24000/1001",
	     "pix_fmt":"yuv420p10le","color_transfer":"smpte2084",
	     "side_data_list":[{"side_data_type":"DOVI configuration record"}]},
	    {"codec_type":"audio","codec_name":"truehd","channels":8,"sample_rate":"48000","tags":{"language":"eng"}},
	    {"codec_type":"audio","codec_name":"ac3","channels":6,"tags":{"language":"fra"}},
	    {"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}},
	    {"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"und"}}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	tech := res.Technical
	if tech.Container != "matroska" || tech.Duration != 8880.5 || tech.VideoCodec != "hevc" || tech.AudioCodec != "truehd" {
		t.Errorf("technical = %+v", tech)
	}
	if tech.HDR != "dolbyvision" || tech.BitDepth != 10 || tech.AudioChannels != 8 || tech.FrameRate != 23.98 {
		t.Errorf("technical = %+v", tech)
	}
	if len(tech.AudioLanguages) != 2 || len(tech.SubtitleLanguages) != 1 {
		t.Errorf("languages = %v / %v", tech.AudioLanguages, tech.SubtitleLanguages)
	}
	if res.Tags["title"] != "Movie" {
		t.Errorf("tags = %v", res.Tags)
	}
}

func TestParseAudioWithArt(t *testing.T) {
	res, err := Parse([]byte(`{
	  "format": {"format_name": "flac", "duration": "201.2", "tags": {"ARTIST": "A", "TRACK": "3/12"}},
	  "streams": [
	    {"codec_type":"audio","codec_name":"flac","channels":2,"sample_rate":"44100","bits_per_raw_sample":"16"},
	    {"codec_type":"video","codec_name":"mjpeg","width":600,"height":600,"disposition":{"attached_pic":1}}
	  ]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasEmbeddedArt || res.Technical.VideoCodec != "" || res.Technical.BitDepth != 16 || res.Technical.SampleRate != 44100 {
		t.Errorf("result = %+v", res)
	}
	if res.Tags["artist"] != "A" || res.Tags["track"] != "3/12" {
		t.Errorf("tags = %v", res.Tags)
	}
}
