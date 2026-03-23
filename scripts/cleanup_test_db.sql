TRUNCATE TABLE
    maintenance_wait_users,
    user_proxies,
    pro_subscriptions,
    pro_groups,
    proxy_nodes,
    users,
    premium_servers,
    vps_provision_requests,
    invoices,
    star_payments,
    ad_pins,
    ads,
    op_channel_clicks,
    app_settings
RESTART IDENTITY CASCADE;

SELECT 'Test DB cleaned' AS status;
