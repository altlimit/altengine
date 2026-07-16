<?php

declare(strict_types=1);

namespace AltEngine;

/**
 * Field builders — F::text('title', 'Blue Shoes') reads better than array
 * literals and pins the right 'type' string.
 */
final class F
{
    public static function text(string $name, string $value, ?string $language = null): array
    {
        return self::mk($name, 'text', $value, $language);
    }

    public static function html(string $name, string $value, ?string $language = null): array
    {
        return self::mk($name, 'html', $value, $language);
    }

    public static function atom(string $name, string $value): array
    {
        return self::mk($name, 'atom', $value);
    }

    public static function number(string $name, float|int $value): array
    {
        return self::mk($name, 'number', $value);
    }

    /** Accepts a DateTimeInterface, ISO string, or epoch milliseconds. */
    public static function date(string $name, \DateTimeInterface|string|int $value): array
    {
        if ($value instanceof \DateTimeInterface) {
            $value = $value->format(\DateTimeInterface::RFC3339);
        }
        return self::mk($name, 'date', $value);
    }

    public static function geo(string $name, float $lat, float $lng): array
    {
        return self::mk($name, 'geo', ['lat' => $lat, 'lng' => $lng]);
    }

    public static function tokenprefix(string $name, string $value): array
    {
        return self::mk($name, 'tokenprefix', $value);
    }

    public static function untokenprefix(string $name, string $value): array
    {
        return self::mk($name, 'untokenprefix', $value);
    }

    // --- facets ---

    public static function atomFacet(string $name, string $value): array
    {
        return ['name' => $name, 'type' => 'atom', 'value' => $value];
    }

    public static function numberFacet(string $name, float|int $value): array
    {
        return ['name' => $name, 'type' => 'number', 'value' => $value];
    }

    private static function mk(string $name, string $type, mixed $value, ?string $language = null): array
    {
        $field = ['name' => $name, 'type' => $type, 'value' => $value];
        if ($language !== null) {
            $field['language'] = $language;
        }
        return $field;
    }
}
