FROM python:3.11-slim

# Step 1: 系统包（禁用代理，直连 deb.debian.org）
ENV http_proxy="" https_proxy="" HTTP_PROXY="" HTTPS_PROXY=""
RUN echo 'Acquire::http::Proxy "false";' > /etc/apt/apt.conf.d/99disableproxy && \
    apt-get clean && apt-get update && apt-get install -y --no-install-recommends \
    xvfb libglib2.0-0 libnss3 libxss1 libasound2 libx11-xcb1 \
    wget ca-certificates fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip config set global.index-url https://pypi.tuna.tsinghua.edu.cn/simple && \
    pip install --no-cache-dir -r requirements.txt

# Step 2: camoufox 浏览器下载（启用代理，走 7891）
ENV http_proxy=http://172.18.45.188:7891
ENV https_proxy=http://172.18.45.188:7891
RUN python -m camoufox fetch

# Step 3: 复制应用代码
COPY . /app/
COPY register-entrypoint.sh /app/register-entrypoint.sh
RUN chmod +x /app/register-entrypoint.sh
ENV DISPLAY=:99
ENTRYPOINT ["/app/register-entrypoint.sh"]
