#define MA_NO_DEVICE_IO
#define MA_NO_THREADING
#define MA_NO_DECODING
#define MA_NO_ENCODING
#define MA_NO_WAV
#define MA_NO_FLAC
#define MA_NO_MP3
#define MA_NO_RESOURCE_MANAGER
#define MA_NO_NODE_GRAPH
#define MA_NO_ENGINE
#define MA_NO_GENERATION
#define MINIAUDIO_IMPLEMENTATION
#include "miniaudio.h"

/* Returns the resampler's address, or a negative ma_result. */
int kolo_rs_new(ma_uint32 channels, ma_uint32 in, ma_uint32 out) {
	ma_resampler *r = calloc(1, sizeof(*r));
	if (r == NULL) return MA_OUT_OF_MEMORY;
	ma_resampler_config c = ma_resampler_config_init(ma_format_s16, channels, in, out, ma_resample_algorithm_linear);
	ma_result res = ma_resampler_init(&c, NULL, r);
	if (res != MA_SUCCESS) { free(r); return res; }
	return (int)(size_t)r;
}

void kolo_rs_free(ma_resampler *r) { ma_resampler_uninit(r, NULL); free(r); }

/* counts[0] is the input frames consumed, counts[1] the output frames written. */
int kolo_rs_process(ma_resampler *r, const void *in, ma_uint32 in_frames, void *out, ma_uint32 out_frames, ma_uint32 *counts) {
	ma_uint64 nin = in_frames, nout = out_frames;
	ma_result res = ma_resampler_process_pcm_frames(r, in, &nin, out, &nout);
	counts[0] = (ma_uint32)nin;
	counts[1] = (ma_uint32)nout;
	return res;
}

ma_uint64 kolo_rs_expected(ma_resampler *r, ma_uint64 in_frames) {
	ma_uint64 out;
	return ma_resampler_get_expected_output_frame_count(r, in_frames, &out) == MA_SUCCESS ? out : 0;
}

ma_uint64 kolo_rs_input_latency(ma_resampler *r) { return ma_resampler_get_input_latency(r); }
ma_uint64 kolo_rs_output_latency(ma_resampler *r) { return ma_resampler_get_output_latency(r); }
int kolo_rs_reset(ma_resampler *r) { return ma_resampler_reset(r); }
void *kolo_malloc(size_t n) { return malloc(n); }
