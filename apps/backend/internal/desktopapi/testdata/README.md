`silence.m4a` is an AAC mono recording generated from 100 ms of synthetic silence
at 16 kHz. It contains no microphone input or speech. It exercises macOS M4A
containers that Go's generic MIME sniffer classifies as `video/mp4`.

The fixture was produced by writing 1,600 zero-valued signed 16-bit PCM samples
to a mono WAV, then running macOS `afconvert -f m4af -d aac input.wav silence.m4a`.
