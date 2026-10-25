#ifndef VIDEO_DAEMON_VIDEO_H
#define VIDEO_DAEMON_VIDEO_H

#include "ctrl.h"

/**
 * @brief Initialize the video subsystem
 *
 * @return int 0 on success, -1 on failure
 */
int video_init(float quality_factor);

/**
 * @brief Shutdown the video subsystem
 */
void video_shutdown();

/**
 * @brief Run the detect format thread
 *
 * @param arg The argument to pass to the thread
 * @return void* The result of the thread
 */
void *run_detect_format(void *arg);

/**
 * @brief Start the video streaming
 */
void video_start_streaming();

/**
 * @brief Stop the video streaming
 */
void video_stop_streaming();

/**
 * @brief Get the streaming status of the video
 *
 * @return uint8_t 1 if the video streaming is active, 2 if the video streaming is stopping, 0 otherwise
 */
uint8_t video_get_streaming_status();

/**
 * @brief Set the quality factor of the video
 *
 * @param factor The quality factor to set
 */
void video_set_quality_factor(float factor);

/**
 * @brief Get the quality factor of the video
 *
 * @return float The quality factor of the video
 */
float video_get_quality_factor();

/**
 * @brief Set the codec type (0 = H.264, 1 = H.265)
 */
void video_set_codec_type(int type);

/**
 * @brief Get the codec type (0 = H.264, 1 = H.265)
 */
int video_get_codec_type();

/**
 * @brief Encode the next captured frame as JPEG
 *
 * Blocks until the capture thread has encoded a frame or the timeout expires.
 * Requires the video stream to be running.
 *
 * @param params Quality, region and size of the snapshot (jetkvm_snapshot_params_t in ctrl.h)
 * @param timeout_ms Maximum time to wait for a frame
 * @param data Receives a malloc'd buffer with the JPEG data, to be freed by the caller
 * @param len Receives the length of the JPEG data
 * @return int 0 on success, one of VIDEO_SNAPSHOT_ERR_* (ctrl.h) on failure
 */
int video_snapshot_jpeg(const jetkvm_snapshot_params_t *params, int timeout_ms, uint8_t **data, size_t *len);

#endif //VIDEO_DAEMON_VIDEO_H
