FROM mcr.microsoft.com/playwright:v1.61.1-noble

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY src ./src

ENV CURSOR_CHROMIUM_LISTEN=127.0.0.1:18901

CMD ["node", "src/main.mjs"]
