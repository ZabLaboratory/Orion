"""AES-GCM envelope roundtrip."""

from orion.services import encryption


def test_encrypt_decrypt_roundtrip() -> None:
    original = "live_abc123XYZverylongtwitchkey"
    ciphertext, nonce = encryption.encrypt(original)
    assert ciphertext != original.encode()
    assert len(nonce) == 12
    assert encryption.decrypt(ciphertext, nonce) == original


def test_two_encryptions_yield_different_ciphertexts() -> None:
    a_ct, a_n = encryption.encrypt("same")
    b_ct, b_n = encryption.encrypt("same")
    assert a_ct != b_ct
    assert a_n != b_n
