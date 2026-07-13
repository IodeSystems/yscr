// PCM capture worklet for streaming STT. Taps the mic graph, resamples the
// context rate down to 16 kHz (oidio's streaming model rate — it does NOT
// resample server-side), converts to signed 16-bit little-endian, and posts
// ~128 ms frames to the main thread, which base64s them into
// input_audio_buffer.append messages on the realtime WebSocket.
//
// Resampling here (rather than forcing new AudioContext({sampleRate:16000}))
// keeps iOS Safari working — it ignores/throws on a forced context rate.
class PCMWorklet extends AudioWorkletProcessor {
  constructor(options) {
    super();
    // Backend-declared input rate (corrallm realtime-stt wants PCM16 @ 24kHz).
    this.target = (options && options.processorOptions && options.processorOptions.targetRate) || 24000;
    this.ratio = sampleRate / this.target; // e.g. 48000/24000 = 2
    this.readPos = 0; // fractional read cursor, carried across process() blocks
    this.buf = [];
    this.frameSamples = 2048; // ~85 ms at 24 kHz → post size
  }

  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (!ch) return true; // no input this block (keep the node alive)

    let i = this.readPos;
    for (; i < ch.length; i += this.ratio) {
      const idx = Math.floor(i);
      const frac = i - idx;
      const s0 = ch[idx];
      const s1 = idx + 1 < ch.length ? ch[idx + 1] : s0;
      let s = s0 + (s1 - s0) * frac; // linear interpolation
      s = s < -1 ? -1 : s > 1 ? 1 : s;
      this.buf.push(s < 0 ? s * 0x8000 : s * 0x7fff);
      if (this.buf.length >= this.frameSamples) {
        const pcm = Int16Array.from(this.buf);
        this.port.postMessage(pcm.buffer, [pcm.buffer]); // transfer, zero-copy
        this.buf.length = 0;
      }
    }
    this.readPos = i - ch.length; // carry the sub-sample remainder
    return true;
  }
}

registerProcessor("pcm-worklet", PCMWorklet);
