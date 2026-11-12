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


def test_ffmpeg_never_reads_mcp_stdin(tmp_path, monkeypatch):
    """The server speaks MCP over stdin; ffmpeg must neither inherit nor read it."""
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")
    calls = []

    def fake_run(cmd, **kwargs):
        calls.append((cmd, kwargs))
        return audio.subprocess.CompletedProcess(cmd, 0, "", "")

    monkeypatch.setattr(audio.subprocess, "run", fake_run)
    audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))
    cmd, kwargs = calls[0]
    assert "-nostdin" in cmd and cmd.index("-nostdin") < cmd.index("-i")
    assert kwargs.get("stdin") is audio.subprocess.DEVNULL


def test_find_ffmpeg_probes_beyond_minimal_path(tmp_path, monkeypatch):
    """A GUI client's minimal PATH lacks Homebrew; the probe list and the env override cover it."""
    probed = tmp_path / "homebrew" / "ffmpeg"
    probed.parent.mkdir()
    probed.write_text("#!/bin/sh\n")
    probed.chmod(0o755)
    monkeypatch.delenv("WHATSAPP_FFMPEG_BIN", raising=False)
    monkeypatch.setenv("PATH", str(tmp_path / "empty"))
    monkeypatch.setattr(audio, "FFMPEG_PROBE_PATHS", (str(tmp_path / "missing"), str(probed)))
    assert audio.find_ffmpeg() == str(probed)

    monkeypatch.setattr(audio, "FFMPEG_PROBE_PATHS", ())
    assert audio.find_ffmpeg() == "ffmpeg"  # not installed: running it gives the clear error

    monkeypatch.setenv("WHATSAPP_FFMPEG_BIN", "/custom/ffmpeg")
    assert audio.find_ffmpeg() == "/custom/ffmpeg"
    calls = []
    monkeypatch.setattr(
        audio.subprocess,
        "run",
        lambda cmd, **kw: calls.append(cmd) or audio.subprocess.CompletedProcess(cmd, 0),
    )
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")
    audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))
    assert calls[0][0] == "/custom/ffmpeg"
