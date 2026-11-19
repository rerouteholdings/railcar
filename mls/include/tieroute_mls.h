/*
 * tieroute-mls: OpenMLS with Tieroute's post-quantum ciphersuite, behind a
 * C interface. See README.md for the requests and responses.
 *
 * Every function that returns int32_t returns 0 on success, or one of the
 * TRMLS_ERR_ codes. On success, *out holds the result: JSON, raw bytes, or
 * nothing, as README.md lists. On failure, it holds the error message in
 * UTF-8. Free *out with trmls_buf_free in both cases.
 *
 * A client handle isn't safe to use from two threads at once. A signer
 * handle is read-only, so it is.
 */

#ifndef TIEROUTE_MLS_H
#define TIEROUTE_MLS_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define TRMLS_OK 0
#define TRMLS_ERR_BAD_INPUT 1
#define TRMLS_ERR_NOT_FOUND 2
#define TRMLS_ERR_INVALID 3
#define TRMLS_ERR_WRONG_EPOCH 4
#define TRMLS_ERR_PENDING_COMMIT 5
#define TRMLS_ERR_PENDING_PROPOSAL 6
#define TRMLS_ERR_INACTIVE 7
#define TRMLS_ERR_PAST_EPOCH 8
#define TRMLS_ERR_STORAGE 9
#define TRMLS_ERR_INTERNAL 10

typedef struct TrmlsClient TrmlsClient;
typedef struct TrmlsSigner TrmlsSigner;

/* A buffer the library allocated. */
typedef struct {
  uint8_t *ptr;
  size_t len;
} TrmlsBuf;

void trmls_buf_free(TrmlsBuf buf);

/* The ciphersuite's code point: 0x004F. */
uint16_t trmls_ciphersuite(void);

/* Signers: an Ed25519 key, from its 32-byte private key. */
int32_t trmls_signer_new(const uint8_t *seed, size_t len, TrmlsSigner **signer, TrmlsBuf *out);
int32_t trmls_signer_public_key(const TrmlsSigner *signer, TrmlsBuf *out);
void trmls_signer_free(TrmlsSigner *signer);

/* Clients: one device's MLS state. An empty snapshot opens a new client. */
int32_t trmls_client_open(const uint8_t *snapshot, size_t len, TrmlsClient **client, TrmlsBuf *out);
void trmls_client_free(TrmlsClient *client);
int32_t trmls_client_snapshot(TrmlsClient *client, TrmlsBuf *out);
int32_t trmls_client_take_changes(TrmlsClient *client, TrmlsBuf *out);
int32_t trmls_client_revert(TrmlsClient *client, TrmlsBuf *out);

/* Key packages. */
int32_t trmls_key_package(TrmlsClient *client, const TrmlsSigner *signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_delete_key_package(TrmlsClient *client, const uint8_t *hash_ref, size_t len, TrmlsBuf *out);

/* Creating, joining, and deleting groups. */
int32_t trmls_create_group(TrmlsClient *client, const TrmlsSigner *signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_join_welcome(TrmlsClient *client, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_join_external(TrmlsClient *client, const TrmlsSigner *signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_delete_group(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);
int32_t trmls_group_info(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);

/* Commits and proposals. new_signer may be NULL. */
int32_t trmls_commit(TrmlsClient *client, const TrmlsSigner *signer, const TrmlsSigner *new_signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_merge_pending_commit(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);
int32_t trmls_clear_pending_commit(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);
int32_t trmls_merge_staged_commit(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);
int32_t trmls_discard_staged_commit(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);
int32_t trmls_propose_remove(TrmlsClient *client, const TrmlsSigner *signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_remove_pending_proposal(TrmlsClient *client, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_clear_pending_proposals(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);

/* Messages. */
int32_t trmls_encrypt(TrmlsClient *client, const TrmlsSigner *signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_process(TrmlsClient *client, const uint8_t *req, size_t len, TrmlsBuf *out);

/* Exports. */
int32_t trmls_export_secret(TrmlsClient *client, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_export_group_info(TrmlsClient *client, const TrmlsSigner *signer, const uint8_t *req, size_t len, TrmlsBuf *out);
int32_t trmls_export_ratchet_tree(TrmlsClient *client, const uint8_t *group_id, size_t len, TrmlsBuf *out);

/* Reading bytes without a client. */
int32_t trmls_inspect_key_package(const uint8_t *bytes, size_t len, TrmlsBuf *out);
int32_t trmls_inspect_group_info(const uint8_t *bytes, size_t len, TrmlsBuf *out);
int32_t trmls_inspect_message(const uint8_t *bytes, size_t len, TrmlsBuf *out);

#ifdef __cplusplus
}
#endif

#endif /* TIEROUTE_MLS_H */
