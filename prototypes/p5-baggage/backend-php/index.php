<?php
// Base backend in PHP (Slim + Guzzle). Context comes from ext-opentelemetry auto-instrumentation.
require __DIR__ . '/vendor/autoload.php';

use Psr\Http\Message\ResponseInterface as Response;
use Psr\Http\Message\ServerRequestInterface as Request;
use Slim\Factory\AppFactory;

$app = AppFactory::create();
$app->get('/api/php/checkout', function (Request $request, Response $response) {
    $client = new GuzzleHttp\Client(['timeout' => 5]);
    $payments = json_decode((string) $client->get('http://payments/payments/charge')->getBody(), true);
    $response->getBody()->write(json_encode([
        'service' => 'backend-php',
        'received_baggage' => $request->getHeaderLine('baggage'),
        'payments' => $payments,
    ]));
    return $response->withHeader('Content-Type', 'application/json');
});
$app->run();
