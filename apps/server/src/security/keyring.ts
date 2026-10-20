// Versioned at-rest encryption (AES-256-GCM) and versioned keyed lookup
// hashes (HMAC-SHA256). Every ciphertext and lookup hash is stored next to
// the key_version that produced it, so a new CURRENT_KEY_VERSION can be
// introduced while older rows remain readable (plan ADR-008).
//
// The sealed format is nonce(12) || ciphertext || tag(16) with the purpose as
// additional data: byte-for-byte the format the Go service wrote.
import { createCipheriv, createDecipheriv, createHmac, randomBytes } from "node:crypto";

// Purposes are bound into ciphertexts as GCM additional data so a ciphertext
// copied from one column cannot be decrypted as another.
export const Purpose = {
  phone: "phone",
  message: "message",
  overflow: "overflow",
} as const;
export type Purpose = (typeof Purpose)[keyof typeof Purpose];

const NONCE = 12;
const TAG = 16;

export class UnknownKeyVersionError extends Error {
  constructor(version: number) {
    super(`security: unknown key version ${version}`);
  }
}

export interface VersionedHash {
  version: number;
  hash: string;
}

export interface Sealed {
  ciphertext: Buffer;
  version: number;
}

export class Keyring {
  private readonly enc = new Map<number, Buffer>();
  private readonly mac = new Map<number, Buffer>();

  /** The current version must have an encryption key (exactly 32 bytes) and an HMAC key (at least 32 bytes). */
  constructor(
    readonly current: number,
    encKeys: Map<number, Buffer>,
    hmacKeys: Map<number, Buffer>,
  ) {
    for (const [v, key] of encKeys) {
      if (key.length !== 32) throw new Error(`security: encryption key v${v} must be 32 bytes`);
      this.enc.set(v, Buffer.from(key));
    }
    for (const [v, key] of hmacKeys) {
      if (key.length < 32) throw new Error(`security: hmac key v${v} must be at least 32 bytes`);
      this.mac.set(v, Buffer.from(key));
    }
    if (!this.enc.has(current)) throw new Error(`security: no encryption key for current version ${current}`);
    if (!this.mac.has(current)) throw new Error(`security: no hmac key for current version ${current}`);
  }

  /** Seals plaintext with the current key. */
  encrypt(purpose: Purpose, plaintext: string | Buffer): Sealed {
    const nonce = randomBytes(NONCE);
    const cipher = createCipheriv("aes-256-gcm", this.enc.get(this.current)!, nonce);
    cipher.setAAD(Buffer.from(purpose));
    const ct = Buffer.concat([cipher.update(plaintext), cipher.final()]);
    return { ciphertext: Buffer.concat([nonce, ct, cipher.getAuthTag()]), version: this.current };
  }

  decrypt(purpose: Purpose, sealed: Uint8Array, version: number): Buffer {
    const key = this.enc.get(version);
    if (!key) throw new UnknownKeyVersionError(version);
    const buf = Buffer.from(sealed);
    if (buf.length < NONCE + TAG) throw new Error("security: ciphertext too short");
    const decipher = createDecipheriv("aes-256-gcm", key, buf.subarray(0, NONCE));
    decipher.setAAD(Buffer.from(purpose));
    decipher.setAuthTag(buf.subarray(buf.length - TAG));
    return Buffer.concat([decipher.update(buf.subarray(NONCE, buf.length - TAG)), decipher.final()]);
  }

  decryptString(purpose: Purpose, sealed: Uint8Array, version: number): string {
    return this.decrypt(purpose, sealed, version).toString("utf8");
  }

  /** hex(HMAC-SHA256(key[version], value)). */
  lookupHash(version: number, value: string): string {
    const key = this.mac.get(version);
    if (!key) throw new UnknownKeyVersionError(version);
    return createHmac("sha256", key).update(value).digest("hex");
  }

  /** The value's hash under every configured HMAC key, current version first, then newest to oldest. */
  lookupHashes(value: string): VersionedHash[] {
    const versions = [...this.mac.keys()].sort((a, b) => {
      if (a === this.current) return -1;
      if (b === this.current) return 1;
      return b - a;
    });
    return versions.map((version) => ({ version, hash: this.lookupHash(version, value) }));
  }
}
