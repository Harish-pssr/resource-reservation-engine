-- The hard guarantee (D2): the database refuses a second confirmed booking for one item,
-- whatever the application or the cache does. Absent from the unprotected negative-control schema.
CREATE UNIQUE INDEX bookings_one_confirmed_per_item ON bookings (item_id) WHERE status = 'confirmed';
