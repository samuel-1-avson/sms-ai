import { describe, expect, it } from "vitest";
import { Keyring, Purpose, UnknownKeyVersionError } from "./keyring";

const key = (b: number) => Buffer.alloc(32, b);
const ring = (current: number, versions: number[]) =>
  new Keyring(
    current,
    new Map(versions.map((v) => [v, key(v)])),
    new Map(versions.map((v) => [v, key(v + 10)])),
  );

describe("Keyring", () => {
  it("decrypts old versions after a rotation and writes with the new one", () => {
    const v1 = ring(1, [1]);
    const sealed = v1.encrypt(Purpose.phone, "+233244123456");
    expect(sealed.version).toBe(1);
    const v2 = ring(2, [1, 2]);
    expect(v2.decryptString(Purpose.phone, sealed.ciphertext, 1)).toBe("+233244123456");
    expect(v2.encrypt(Purpose.phone, "x").version).toBe(2);
  });

  it("binds the purpose", () => {
    const k = ring(1, [1]);
    const sealed = k.encrypt(Purpose.phone, "secret");
    expect(() => k.decrypt(Purpose.message, sealed.ciphertext, sealed.version)).toThrow();
  });

  it("rejects unknown versions", () => {
    expect(() => ring(1, [1]).decrypt(Purpose.phone, Buffer.alloc(40), 9)).toThrow(UnknownKeyVersionError);
  });

  it("orders lookup hashes current first", () => {
    const k = ring(2, [1, 2]);
    const hs = k.lookupHashes("+233244123456");
    expect(hs.map((h) => h.version)).toEqual([2, 1]);
    expect(hs[0]!.hash).not.toBe(hs[1]!.hash);
    expect(k.lookupHash(1, "+233244123456")).toBe(hs[1]!.hash);
  });

  it("validates key sizes", () => {
    expect(() => new Keyring(1, new Map([[1, Buffer.alloc(16)]]), new Map([[1, key(1)]]))).toThrow(/32 bytes/);
    expect(() => new Keyring(1, new Map([[1, key(1)]]), new Map([[1, Buffer.alloc(8)]]))).toThrow(/at least 32/);
    expect(() => new Keyring(2, new Map([[1, key(1)]]), new Map([[1, key(1)]]))).toThrow(/current version 2/);
  });
});
