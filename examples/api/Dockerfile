FROM python:3.12-slim
WORKDIR /app
COPY app.py .
ENV PYTHONUNBUFFERED=1
# Numeric user: shpyrd runs processes as non-root and Kubernetes verifies by uid.
USER 1000
EXPOSE 8080
CMD ["python", "app.py"]
