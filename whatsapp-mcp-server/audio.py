import os
import shutil
import subprocess
import tempfile

# GUI MCP clients often start with a minimal PATH that lacks Homebrew and /usr/local.
FFMPEG_PROBE_PATHS = ("/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg")


def find_ffmpeg() -> str:
    """ffmpeg to run: WHATSAPP_FFMPEG_BIN, PATH, then common install locations.

    Falls back to plain "ffmpeg", which fails with FileNotFoundError when it is not installed.
    """
    explicit = os.environ.get("WHATSAPP_FFMPEG_BIN")
    if explicit:
        return explicit
    found = shutil.which("ffmpeg")
    if found:
        return found
    for candidate in FFMPEG_PROBE_PATHS:
        if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    return "ffmpeg"


def convert_to_opus_ogg(input_file, output_file=None, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container.

    Args:
        input_file (str): Path to the input audio file
        output_file (str, optional): Path to save the output file. If None, replaces the
                                    extension of input_file with .ogg
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)

    Returns:
        str: Path to the converted file

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")

    # If no output file is specified, replace the extension with .ogg
    if output_file is None:
        output_file = os.path.splitext(input_file)[0] + ".ogg"

    # Ensure the output directory exists
    output_dir = os.path.dirname(output_file)
    if output_dir and not os.path.exists(output_dir):
        os.makedirs(output_dir)

    # Build the ffmpeg command
    cmd = [
        find_ffmpeg(),
        "-nostdin",  # never read the MCP stdio stream for interactive commands
        "-i",
        input_file,
        "-c:a",
        "libopus",
        "-b:a",
        bitrate,
        "-ar",
        str(sample_rate),
        "-application",
        "voip",  # Optimize for voice
        "-vbr",
        "on",  # Variable bitrate
        "-compression_level",
        "10",  # Maximum compression
        "-frame_duration",
        "60",  # 60ms frames (good for voice)
        "-y",  # Overwrite output file if it exists
        output_file,
    ]

    try:
        # Run the ffmpeg command and capture output
        # stdin is the MCP JSON-RPC stream; ffmpeg must not inherit it
        subprocess.run(cmd, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True)
        return output_file
    except FileNotFoundError as e:
        # subprocess raises this when the ffmpeg executable itself is missing
        raise FileNotFoundError(
            "ffmpeg not found on PATH or in common install locations. Install ffmpeg, or set "
            "WHATSAPP_FFMPEG_BIN to its absolute path, to send non-.ogg audio."
        ) from e
    except subprocess.CalledProcessError as e:
        raise RuntimeError(
            f"Failed to convert audio. You likely need to install ffmpeg {e.stderr}"
        ) from e


def convert_to_opus_ogg_temp(input_file, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container and store in a temporary file.

    Args:
        input_file (str): Path to the input audio file
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)

    Returns:
        str: Path to the temporary file with the converted audio

    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    # Create a temporary file with .ogg extension
    temp_file = tempfile.NamedTemporaryFile(suffix=".ogg", delete=False)
    temp_file.close()

    try:
        # Convert the audio
        convert_to_opus_ogg(input_file, temp_file.name, bitrate, sample_rate)
        return temp_file.name
    except Exception as e:
        # Clean up the temporary file if conversion fails
        if os.path.exists(temp_file.name):
            os.unlink(temp_file.name)
        raise e


if __name__ == "__main__":
    # Example usage
    import sys

    if len(sys.argv) < 2:
        print("Usage: python audio.py input_file [output_file]")
        sys.exit(1)

    input_file = sys.argv[1]

    try:
        result = convert_to_opus_ogg_temp(input_file)
        print(f"Successfully converted to: {result}")
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
