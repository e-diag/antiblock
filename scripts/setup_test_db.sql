-- psql -d antiblock_test -f scripts/setup_test_db.sql
-- После первого запуска бота (AutoMigrate) или вручную создайте БД.

INSERT INTO users (tg_id, username, is_premium, created_at, updated_at)
VALUES (100000001, 'test_free_user', false, NOW(), NOW())
ON CONFLICT (tg_id) DO NOTHING;

INSERT INTO users (tg_id, username, is_premium, premium_until, created_at, updated_at)
VALUES (100000002, 'test_legacy_active', true, NOW() + interval '30 days', NOW(), NOW())
ON CONFLICT (tg_id) DO NOTHING;

INSERT INTO proxy_nodes (ip, port, secret, type, status, owner_id, timeweb_floating_ip_id, created_at, updated_at)
SELECT '185.104.113.242', 20001, 'dd' || md5(random()::text),
       'premium', 'active', u.id, '', NOW(), NOW()
FROM users u WHERE u.tg_id = 100000002
  AND NOT EXISTS (SELECT 1 FROM proxy_nodes p WHERE p.owner_id = u.id AND p.type = 'premium');

INSERT INTO users (tg_id, username, is_premium, premium_until, created_at, updated_at)
VALUES (100000003, 'test_legacy_expired', true, NOW() - interval '1 minute', NOW(), NOW())
ON CONFLICT (tg_id) DO NOTHING;

INSERT INTO proxy_nodes (ip, port, secret, type, status, owner_id, timeweb_floating_ip_id, created_at, updated_at)
SELECT '185.104.113.242', 20002, 'dd' || md5(random()::text),
       'premium', 'active', u.id, '', NOW(), NOW()
FROM users u WHERE u.tg_id = 100000003
  AND NOT EXISTS (SELECT 1 FROM proxy_nodes p WHERE p.owner_id = u.id AND p.type = 'premium');

INSERT INTO proxy_nodes (ip, port, secret, type, status, load, created_at, updated_at)
SELECT '185.104.113.242', 443, 'dd1234567890abcdef1234567890abcdef', 'free', 'active', 5, NOW(), NOW()
WHERE NOT EXISTS (
  SELECT 1 FROM proxy_nodes WHERE ip = '185.104.113.242' AND port = 443
    AND secret = 'dd1234567890abcdef1234567890abcdef'
);

INSERT INTO proxy_nodes (ip, port, secret, type, status, load, created_at, updated_at)
SELECT '185.104.113.242', 8080, 'dd' || md5('test2'), 'free', 'active', 2, NOW(), NOW()
WHERE NOT EXISTS (
  SELECT 1 FROM proxy_nodes WHERE ip = '185.104.113.242' AND port = 8080
);

INSERT INTO premium_servers (name, ip, timeweb_id, is_active, docker_port, created_at, updated_at)
SELECT 'test-premium-server', '1.2.3.4', 0, true, 22, NOW(), NOW()
WHERE NOT EXISTS (SELECT 1 FROM premium_servers WHERE name = 'test-premium-server');

SELECT 'Test data created' AS status;
SELECT 'users' AS t, COUNT(*)::text AS n FROM users
UNION ALL SELECT 'proxy_nodes', COUNT(*)::text FROM proxy_nodes;
