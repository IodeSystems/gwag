// kafkajs reads gzip and nothing else. franz-go, which the Go backend
// uses, compresses with Snappy by default, so a TypeScript consumer of
// a topic a Go service writes needs a Snappy codec. This one reads
// raw Snappy blocks (franz-go, librdkafka) and the xerial framing the
// Java client writes. LZ4 and ZSTD are not provided: register your own
// in kafkajs's CompressionCodecs.
import kafkajs from "kafkajs";
// snappyjs ships no types.
// @ts-expect-error untyped
import snappy from "snappyjs";

const { CompressionCodecs, CompressionTypes } = kafkajs;

const xerialMagic = Buffer.from([0x82, 0x53, 0x4e, 0x41, 0x50, 0x50, 0x59, 0x00]);

/** Decompresses a Kafka Snappy payload: one raw block, or xerial-framed blocks. */
export function snappyDecompress(buf: Buffer): Buffer {
  if (buf.length >= 16 && buf.subarray(0, 8).equals(xerialMagic)) {
    const out: Buffer[] = [];
    for (let at = 16; at < buf.length; ) {
      const n = buf.readUInt32BE(at);
      at += 4;
      out.push(Buffer.from(snappy.uncompress(buf.subarray(at, at + n))));
      at += n;
    }
    return Buffer.concat(out);
  }
  return Buffer.from(snappy.uncompress(buf));
}

/** Raw Snappy block, which every Kafka client reads. */
export function snappyCompress(buf: Buffer): Buffer {
  return Buffer.from(snappy.compress(buf));
}

let installed = false;

/**
 * Registers the Snappy codec with kafkajs, unless a codec is there
 * already (kafkajs's own throws when called). KafkaBackend calls it.
 */
export function installSnappy(): void {
  if (installed) return;
  installed = true;
  const codecs = CompressionCodecs as Record<number, () => unknown>;
  try {
    codecs[CompressionTypes.Snappy]?.();
    return; // somebody registered a working one
  } catch {
    // kafkajs's "not implemented"
  }
  codecs[CompressionTypes.Snappy] = () => ({
    compress: async (encoder: { buffer: Buffer }) => snappyCompress(encoder.buffer),
    decompress: async (buffer: Buffer) => snappyDecompress(buffer),
  });
}
