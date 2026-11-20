#include <stdlib.h>
#include "opus.h"

/* Non-variadic entry points: every integer ctl goes through one set and one get. */
int kolo_enc_set(OpusEncoder *e, int req, opus_int32 v) { return opus_encoder_ctl(e, req, v); }
int kolo_enc_get(OpusEncoder *e, int req, opus_int32 *v) { return opus_encoder_ctl(e, req, v); }
int kolo_enc_get_u32(OpusEncoder *e, int req, opus_uint32 *v) { return opus_encoder_ctl(e, req, v); }
int kolo_enc_reset(OpusEncoder *e) { return opus_encoder_ctl(e, OPUS_RESET_STATE); }
int kolo_dec_set(OpusDecoder *d, int req, opus_int32 v) { return opus_decoder_ctl(d, req, v); }
int kolo_dec_get(OpusDecoder *d, int req, opus_int32 *v) { return opus_decoder_ctl(d, req, v); }
int kolo_dec_get_u32(OpusDecoder *d, int req, opus_uint32 *v) { return opus_decoder_ctl(d, req, v); }
int kolo_dec_reset(OpusDecoder *d) { return opus_decoder_ctl(d, OPUS_RESET_STATE); }
void *kolo_malloc(size_t n) { return malloc(n); }
void kolo_free(void *p) { free(p); }
