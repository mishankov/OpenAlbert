FROM python:3.13-slim-trixie

RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*
COPY services/tts/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt && rm /tmp/requirements.txt

# Pin weights and phonemization settings from the same Piper voice revision.
ADD --chmod=0444 --checksum=sha256:9ed9941bac7373f2ec1be36627e0e71a349a59ca5522d4363fd50701db05193a https://huggingface.co/rhasspy/piper-voices/resolve/2f358b6a560e3a913ebf509b2f3344f20b367f94/ru/ru_RU/dmitri/medium/ru_RU-dmitri-medium.onnx /models/ru_RU-dmitri-medium.onnx
ADD --chmod=0444 --checksum=sha256:b7ed744f65ec73deb57fc8e419a5db5c0c5532026cf1cd21247f75937b6fbee2 https://huggingface.co/rhasspy/piper-voices/resolve/2f358b6a560e3a913ebf509b2f3344f20b367f94/ru/ru_RU/dmitri/medium/ru_RU-dmitri-medium.onnx.json /models/ru_RU-dmitri-medium.onnx.json
ADD --chmod=0444 --checksum=sha256:4da875aca6642bd96f0327705e874cbbcc44bc1790ee1d08b3fc0eb266523e5d https://huggingface.co/rhasspy/piper-voices/resolve/2f358b6a560e3a913ebf509b2f3344f20b367f94/ru/ru_RU/dmitri/medium/MODEL_CARD /models/MODEL_CARD
RUN chmod 0755 /models

COPY services/tts/server.py /usr/local/bin/tts-server.py
ENV PYTHONUTF8=1 PYTHONDONTWRITEBYTECODE=1
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["python3", "/usr/local/bin/tts-server.py"]
