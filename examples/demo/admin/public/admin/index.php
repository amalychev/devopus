<?php
// The demo's admin page: PHP calls the API inside the environment by its logical name.
$items = json_decode(file_get_contents(getenv('API_URL') . '/api/items'), true);
?><!doctype html><meta charset=utf-8><title>Admin · Devcorb demo</title>
<style>body{font:16px/1.5 system-ui;max-width:40rem;margin:3rem auto;padding:0 1rem}</style>
<h1>Admin</h1>
<p>PHP <?= PHP_VERSION ?> read <?= count($items) ?> items from <code><?= htmlspecialchars(getenv('API_URL')) ?></code>.</p>
<p><a href="/">Back</a></p>
