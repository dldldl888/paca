CREATE TABLE authorization_delivery_state (
 instance_id uuid PRIMARY KEY,
 epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
 blocked boolean NOT NULL DEFAULT true
);
CREATE TABLE authorization_delivery_participants (
 instance_id uuid NOT NULL REFERENCES authorization_delivery_state(instance_id),
 boot_id uuid NOT NULL,
 kind text NOT NULL CHECK (kind IN ('api','runner','realtime')),
 PRIMARY KEY (instance_id, boot_id)
);
