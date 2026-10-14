FROM node:22-alpine
WORKDIR /app
COPY package.json .
RUN npm install --omit=dev --no-audit --no-fund
COPY server.js .
CMD ["node", "--require", "@opentelemetry/auto-instrumentations-node/register", "server.js"]
