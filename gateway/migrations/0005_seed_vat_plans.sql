INSERT INTO plans (id, display_name, rate_limit_per_minute, monthly_unit_cap, stripe_price_id) VALUES
    ('vat_free',     'VAT Free',     60,    100,   NULL),
    ('vat_pro',      'VAT Pro',      600,   10000, NULL),
    ('vat_business', 'VAT Business', 6000,  NULL,  NULL)
ON CONFLICT (id) DO NOTHING;
