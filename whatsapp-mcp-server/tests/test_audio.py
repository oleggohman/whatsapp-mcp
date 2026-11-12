"""send_audio_message temp-file cleanup and ffmpeg handling (ffmpeg itself is monkeypatched)."""

import os

import pytest

import audio
import whatsapp


@pytest.fixture
def sent(monkeypatch):
    calls = []

    def fake_send(payload):
        calls.append(payload)
        assert os.path.isfile(payload["media_path"])  # file must still exist while sending
        return True, "sent"

    monkeypatch.setattr(whatsapp, "_send", fake_send)
    return calls


def test_converted_temp_file_removed_after_send(tmp_path, monkeypatch, sent):
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")
    temp = tmp_path / "converted.ogg"

    def fake_convert(path):
        temp.write_bytes(b"OggS")
        return str(temp)

    monkeypatch.setattr(audio, "convert_to_opus_ogg_temp", fake_convert)
    assert whatsapp.send_audio_message("123", str(src)) == (True, "sent")
    assert sent[0]["media_path"] == str(temp)
    assert not temp.exists()
    assert src.exists()


def test_temp_file_removed_when_send_raises(tmp_path, monkeypatch):
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")
    temp = tmp_path / "converted.ogg"

    def fake_convert(path):
        temp.write_bytes(b"OggS")
        return str(temp)

    def boom(payload):
        raise RuntimeError("send failed")

    monkeypatch.setattr(audio, "convert_to_opus_ogg_temp", fake_convert)
    monkeypatch.setattr(whatsapp, "_send", boom)
    with pytest.raises(RuntimeError):
        whatsapp.send_audio_message("123", str(src))
    assert not temp.exists()


def test_ogg_input_is_not_deleted(tmp_path, sent):
    src = tmp_path / "voice.ogg"
    src.write_bytes(b"OggS")
    assert whatsapp.send_audio_message("123", str(src)) == (True, "sent")
    assert src.exists()


def test_ffmpeg_missing_gives_clear_error(tmp_path, monkeypatch):
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")

    def no_ffmpeg(*args, **kwargs):
        raise FileNotFoundError(2, "No such file or directory", "ffmpeg")

    monkeypatch.setattr(audio.subprocess, "run", no_ffmpeg)
    ok, msg = whatsapp.send_audio_message("123", str(src))
    assert ok is False and "ffmpeg not found" in msg


def test_ffmpeg_missing_leaves_no_temp_file(tmp_path, monkeypatch):
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")
    monkeypatch.setenv("TMPDIR", str(tmp_path / "t"))
    (tmp_path / "t").mkdir()
    monkeypatch.setattr(audio.tempfile, "tempdir", str(tmp_path / "t"))

    def no_ffmpeg(*args, **kwargs):
        raise FileNotFoundError(2, "No such file or directory", "ffmpeg")

    monkeypatch.setattr(audio.subprocess, "run", no_ffmpeg)
    with pytest.raises(FileNotFoundError, match="ffmpeg not found"):
        audio.convert_to_opus_ogg_temp(str(src))
    assert list((tmp_path / "t").iterdir()) == []
