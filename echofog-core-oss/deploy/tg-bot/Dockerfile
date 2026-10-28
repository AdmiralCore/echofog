# The pass bot of an EchoFog node (see README.md). Built and run by deploy/docker-compose.bot.yml.
FROM python:3.12-slim
WORKDIR /bot
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && useradd --uid 10001 bot
COPY bot.py .
USER bot
CMD ["python", "-u", "bot.py"]
