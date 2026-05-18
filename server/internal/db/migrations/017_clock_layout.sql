-- Saison 14-04-Phase2-FIX05: Mieter-Praeferenz fuer das
-- Bildschirmschoner-Uhr-Layout.
--
--   clock_layout = "horizontal"  klassisches HH:MM mit Doppelpunkt
--                = "vertical"    Pixel-Style aus FIX04: HH ueber MM
--                                 ohne Trenner
--                = NULL          Code-Default "vertical" via Resolver
--
-- Nullable TEXT analog zu language / idle_view_mode in den
-- vorherigen Settings-Migrationen. Keine DDL-Defaults; der
-- Resolver in mockmanager liefert konsistente Defaults und
-- behaelt das "noch nie gesetzt"-Signal in der DB.

ALTER TABLE viewers ADD COLUMN clock_layout TEXT;

INSERT INTO schema_version (version, applied_at)
VALUES (17, strftime('%s','now')*1000);
