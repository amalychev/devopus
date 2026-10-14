FROM php:8.3-cli
RUN apt-get update && apt-get install -y --no-install-recommends unzip git && rm -rf /var/lib/apt/lists/* \
 && pecl install opentelemetry-1.1.0 && docker-php-ext-enable opentelemetry
COPY --from=composer:2.8 /usr/bin/composer /usr/bin/composer
WORKDIR /app
COPY composer.json .
RUN composer install --no-dev --no-interaction --no-progress
COPY index.php .
CMD ["php", "-S", "0.0.0.0:80", "index.php"]
