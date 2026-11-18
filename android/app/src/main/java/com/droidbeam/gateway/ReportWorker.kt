package com.droidbeam.gateway

import android.content.Context
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.IOException
import java.util.concurrent.TimeUnit

/**
 * Uploads status reports and incoming SMS to the server.
 * Runs through WorkManager so reports survive network outages and process death.
 */
class ReportWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        val prefs = Prefs(applicationContext)
        if (!prefs.isRegistered) return@withContext Result.failure()
        val api = ApiClient.forPrefs(prefs)
        val data = inputData
        try {
            when (data.getString(KEY_TYPE)) {
                TYPE_STATUS -> api.reportStatus(
                    data.getLong(KEY_MESSAGE_ID, 0),
                    data.getString(KEY_STATUS)!!,
                    data.getString(KEY_ERROR),
                    data.getInt(KEY_SIM, 0).takeIf { it > 0 },
                )
                TYPE_INCOMING -> api.reportIncoming(
                    data.getString(KEY_UID)!!,
                    data.getString(KEY_FROM)!!,
                    data.getString(KEY_BODY).orEmpty(),
                    data.getLong(KEY_RECEIVED_AT, 0),
                    data.getInt(KEY_SIM, 0).takeIf { it > 0 },
                )
                else -> return@withContext Result.failure()
            }
            Result.success()
        } catch (e: ApiException) {
            if (e.isClientError) {
                // Retrying will not help (e.g. message unknown, token revoked).
                GatewayState.log("Report rejected by server: ${e.message}")
                Result.failure()
            } else {
                Result.retry()
            }
        } catch (e: IOException) {
            Result.retry()
        }
    }

    companion object {
        private const val KEY_TYPE = "type"
        private const val TYPE_STATUS = "status"
        private const val TYPE_INCOMING = "incoming"
        private const val KEY_MESSAGE_ID = "message_id"
        private const val KEY_STATUS = "status"
        private const val KEY_ERROR = "error"
        private const val KEY_UID = "uid"
        private const val KEY_FROM = "from"
        private const val KEY_BODY = "body"
        private const val KEY_RECEIVED_AT = "received_at"
        private const val KEY_SIM = "sim"

        private val constraints = Constraints.Builder()
            .setRequiredNetworkType(NetworkType.CONNECTED)
            .build()

        fun enqueueStatus(context: Context, messageId: Long, status: String, error: String? = null, sim: Int? = null) {
            val request = OneTimeWorkRequestBuilder<ReportWorker>()
                .setInputData(
                    workDataOf(
                        KEY_TYPE to TYPE_STATUS,
                        KEY_MESSAGE_ID to messageId,
                        KEY_STATUS to status,
                        KEY_ERROR to error,
                        KEY_SIM to (sim ?: 0),
                    )
                )
                .setConstraints(constraints)
                .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 10, TimeUnit.SECONDS)
                .build()
            // Chain reports per message so "sent" always reaches the server before "delivered".
            WorkManager.getInstance(context)
                .enqueueUniqueWork("status-$messageId", ExistingWorkPolicy.APPEND_OR_REPLACE, request)
        }

        fun enqueueIncoming(context: Context, uid: String, from: String, body: String, receivedAt: Long, sim: Int?) {
            val request = OneTimeWorkRequestBuilder<ReportWorker>()
                .setInputData(
                    workDataOf(
                        KEY_TYPE to TYPE_INCOMING,
                        KEY_UID to uid,
                        KEY_FROM to from,
                        KEY_BODY to body,
                        KEY_RECEIVED_AT to receivedAt,
                        KEY_SIM to (sim ?: 0),
                    )
                )
                .setConstraints(constraints)
                .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 10, TimeUnit.SECONDS)
                .build()
            WorkManager.getInstance(context)
                .enqueueUniqueWork("incoming-$uid", ExistingWorkPolicy.KEEP, request)
        }
    }
}
