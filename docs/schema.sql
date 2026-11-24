/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
/*!40101 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;

DROP TABLE IF EXISTS `advanced_settings`;
CREATE TABLE `advanced_settings` (
  `id` tinyint(3) unsigned NOT NULL,
  `rules` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`rules`)),
  `updated_by` bigint(20) unsigned DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `advanced_settings_updated_by_foreign` (`updated_by`),
  CONSTRAINT `advanced_settings_updated_by_foreign` FOREIGN KEY (`updated_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `advanced_settings_logs`;
CREATE TABLE `advanced_settings_logs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `at` timestamp NOT NULL DEFAULT current_timestamp(),
  `by_id` bigint(20) unsigned NOT NULL,
  `by_name` varchar(120) NOT NULL,
  `by_name_en` varchar(120) DEFAULT NULL,
  `how` varchar(20) DEFAULT NULL,
  `changes` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`changes`)),
  PRIMARY KEY (`id`),
  KEY `advanced_settings_logs_by_id_foreign` (`by_id`),
  KEY `advanced_settings_logs_at_index` (`at`),
  CONSTRAINT `advanced_settings_logs_by_id_foreign` FOREIGN KEY (`by_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `announcement_reads`;
CREATE TABLE `announcement_reads` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `announcement_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `announcement_reads_announcement_id_user_id_unique` (`announcement_id`,`user_id`),
  KEY `announcement_reads_user_id_foreign` (`user_id`),
  KEY `announcement_reads_announcement_id_at_index` (`announcement_id`,`at`),
  CONSTRAINT `announcement_reads_announcement_id_foreign` FOREIGN KEY (`announcement_id`) REFERENCES `announcements` (`id`) ON DELETE CASCADE,
  CONSTRAINT `announcement_reads_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=8 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `announcements`;
CREATE TABLE `announcements` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `title` varchar(160) NOT NULL,
  `body` text NOT NULL,
  `important` tinyint(1) NOT NULL DEFAULT 0,
  `author_id` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `announcements_author_id_foreign` (`author_id`),
  KEY `announcements_important_created_at_index` (`important`,`created_at`),
  KEY `announcements_important_index` (`important`),
  KEY `announcements_created_at_index` (`created_at`),
  CONSTRAINT `announcements_author_id_foreign` FOREIGN KEY (`author_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=8 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `app_settings`;
CREATE TABLE `app_settings` (
  `id` tinyint(3) unsigned NOT NULL,
  `due_soon_days` tinyint(3) unsigned NOT NULL DEFAULT 1,
  `lock_after` tinyint(3) unsigned NOT NULL DEFAULT 5,
  `lock_minutes` smallint(5) unsigned NOT NULL DEFAULT 15,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
INSERT INTO `app_settings` VALUES
(1,1,5,15,'2026-11-21 07:19:41','2026-11-21 07:19:41');

DROP TABLE IF EXISTS `attachments`;
CREATE TABLE `attachments` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `owner_type` enum('task','project','bug','message') NOT NULL,
  `owner_id` bigint(20) unsigned NOT NULL,
  `name` varchar(200) NOT NULL,
  `size` bigint(20) unsigned NOT NULL,
  `mime` varchar(120) NOT NULL,
  `voice` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`voice`)),
  `disk_path` varchar(255) NOT NULL,
  `uploaded_by` bigint(20) unsigned NOT NULL,
  `at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `attachments_uploaded_by_foreign` (`uploaded_by`),
  KEY `attachments_owner_type_owner_id_index` (`owner_type`,`owner_id`),
  CONSTRAINT `attachments_uploaded_by_foreign` FOREIGN KEY (`uploaded_by`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=9 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `audit_logs`;
CREATE TABLE `audit_logs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `actor_id` bigint(20) unsigned DEFAULT NULL,
  `action` varchar(64) NOT NULL,
  `target_type` varchar(32) NOT NULL,
  `target_id` bigint(20) unsigned DEFAULT NULL,
  `target_label` varchar(255) NOT NULL,
  `changes` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`changes`)),
  `at` timestamp NOT NULL DEFAULT current_timestamp(),
  `ip` varchar(45) DEFAULT NULL,
  `user_agent` varchar(300) DEFAULT NULL,
  `session_id` varchar(64) DEFAULT NULL,
  `request_id` varchar(64) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `audit_logs_actor_id_foreign` (`actor_id`),
  KEY `audit_logs_target_type_target_id_index` (`target_type`,`target_id`),
  KEY `audit_logs_action_index` (`action`),
  KEY `audit_logs_at_index` (`at`),
  KEY `audit_logs_ip_index` (`ip`),
  KEY `audit_logs_session_id_index` (`session_id`),
  KEY `audit_logs_request_id_index` (`request_id`),
  CONSTRAINT `audit_logs_actor_id_foreign` FOREIGN KEY (`actor_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=19 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `auth_sessions`;
CREATE TABLE `auth_sessions` (
  `id` varchar(64) NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `token_id` bigint(20) unsigned DEFAULT NULL,
  `ip` varchar(45) DEFAULT NULL,
  `user_agent` varchar(300) DEFAULT NULL,
  `started_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `last_seen_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `ended_at` timestamp NULL DEFAULT NULL,
  `ended_by` bigint(20) unsigned DEFAULT NULL,
  `end_reason` varchar(24) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `auth_sessions_ended_by_foreign` (`ended_by`),
  KEY `auth_sessions_user_id_ended_at_index` (`user_id`,`ended_at`),
  KEY `auth_sessions_token_id_index` (`token_id`),
  KEY `auth_sessions_last_seen_at_index` (`last_seen_at`),
  KEY `auth_sessions_ended_at_index` (`ended_at`),
  CONSTRAINT `auth_sessions_ended_by_foreign` FOREIGN KEY (`ended_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  CONSTRAINT `auth_sessions_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `avatars`;
CREATE TABLE `avatars` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint(20) unsigned NOT NULL,
  `disk_path` varchar(255) NOT NULL,
  `mime` varchar(64) NOT NULL,
  `size` int(10) unsigned NOT NULL,
  `width` smallint(5) unsigned NOT NULL,
  `height` smallint(5) unsigned NOT NULL,
  `cache_key` varchar(64) NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `avatars_cache_key_unique` (`cache_key`),
  KEY `avatars_user_id_index` (`user_id`),
  CONSTRAINT `avatars_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `bug_events`;
CREATE TABLE `bug_events` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `bug_id` bigint(20) unsigned NOT NULL,
  `actor_id` bigint(20) unsigned NOT NULL,
  `kind` varchar(40) NOT NULL,
  `from` varchar(255) DEFAULT NULL,
  `to` varchar(255) DEFAULT NULL,
  `note` varchar(300) DEFAULT NULL,
  `at` timestamp NOT NULL,
  PRIMARY KEY (`id`),
  KEY `bug_events_actor_id_foreign` (`actor_id`),
  KEY `bug_events_bug_id_at_index` (`bug_id`,`at`),
  KEY `bug_events_at_index` (`at`),
  CONSTRAINT `bug_events_actor_id_foreign` FOREIGN KEY (`actor_id`) REFERENCES `users` (`id`),
  CONSTRAINT `bug_events_bug_id_foreign` FOREIGN KEY (`bug_id`) REFERENCES `bugs` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=23 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `bugs`;
CREATE TABLE `bugs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `code` varchar(32) NOT NULL,
  `project_id` bigint(20) unsigned NOT NULL,
  `phase_id` bigint(20) unsigned DEFAULT NULL,
  `task_id` bigint(20) unsigned DEFAULT NULL,
  `title` varchar(160) NOT NULL,
  `description` text NOT NULL,
  `severity` enum('critical','high','medium','low') NOT NULL,
  `priority` enum('high','medium','low') NOT NULL,
  `status` enum('reported','assigned','fixing','testing','reopened','closed') NOT NULL DEFAULT 'reported',
  `reported_by` bigint(20) unsigned NOT NULL,
  `assignee_id` bigint(20) unsigned DEFAULT NULL,
  `due` date DEFAULT NULL,
  `closed_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `bugs_code_unique` (`code`),
  KEY `bugs_phase_id_foreign` (`phase_id`),
  KEY `bugs_task_id_foreign` (`task_id`),
  KEY `bugs_reported_by_foreign` (`reported_by`),
  KEY `bugs_project_id_status_index` (`project_id`,`status`),
  KEY `bugs_assignee_id_due_index` (`assignee_id`,`due`),
  KEY `bugs_severity_index` (`severity`),
  KEY `bugs_status_index` (`status`),
  KEY `bugs_due_index` (`due`),
  CONSTRAINT `bugs_assignee_id_foreign` FOREIGN KEY (`assignee_id`) REFERENCES `users` (`id`),
  CONSTRAINT `bugs_phase_id_foreign` FOREIGN KEY (`phase_id`) REFERENCES `project_phases` (`id`) ON DELETE SET NULL,
  CONSTRAINT `bugs_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`),
  CONSTRAINT `bugs_reported_by_foreign` FOREIGN KEY (`reported_by`) REFERENCES `users` (`id`),
  CONSTRAINT `bugs_task_id_foreign` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB AUTO_INCREMENT=13 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `cache`;
CREATE TABLE `cache` (
  `key` varchar(255) NOT NULL,
  `value` mediumtext NOT NULL,
  `expiration` int(11) NOT NULL,
  PRIMARY KEY (`key`),
  KEY `cache_expiration_index` (`expiration`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `cache_locks`;
CREATE TABLE `cache_locks` (
  `key` varchar(255) NOT NULL,
  `owner` varchar(255) NOT NULL,
  `expiration` int(11) NOT NULL,
  PRIMARY KEY (`key`),
  KEY `cache_locks_expiration_index` (`expiration`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `celebration_recipients`;
CREATE TABLE `celebration_recipients` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `celebration_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `seen_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `celebration_recipients_celebration_id_user_id_unique` (`celebration_id`,`user_id`),
  KEY `celebration_recipients_user_id_seen_at_index` (`user_id`,`seen_at`),
  CONSTRAINT `celebration_recipients_celebration_id_foreign` FOREIGN KEY (`celebration_id`) REFERENCES `celebrations` (`id`) ON DELETE CASCADE,
  CONSTRAINT `celebration_recipients_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=8 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `celebrations`;
CREATE TABLE `celebrations` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `team_id` bigint(20) unsigned NOT NULL,
  `kind` enum('good','big') NOT NULL,
  `message` varchar(200) DEFAULT NULL,
  `created_by` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `celebrations_created_by_foreign` (`created_by`),
  KEY `celebrations_team_id_created_at_index` (`team_id`,`created_at`),
  KEY `celebrations_created_at_index` (`created_at`),
  CONSTRAINT `celebrations_created_by_foreign` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`),
  CONSTRAINT `celebrations_team_id_foreign` FOREIGN KEY (`team_id`) REFERENCES `teams` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=4 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `chat_group_members`;
CREATE TABLE `chat_group_members` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `group_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `position` int(10) unsigned NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  UNIQUE KEY `chat_group_members_group_id_user_id_unique` (`group_id`,`user_id`),
  KEY `chat_group_members_group_id_position_index` (`group_id`,`position`),
  KEY `chat_group_members_user_id_index` (`user_id`),
  CONSTRAINT `chat_group_members_group_id_foreign` FOREIGN KEY (`group_id`) REFERENCES `chat_groups` (`id`) ON DELETE CASCADE,
  CONSTRAINT `chat_group_members_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=12 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `chat_groups`;
CREATE TABLE `chat_groups` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(60) NOT NULL,
  `created_by` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `chat_groups_created_by_foreign` (`created_by`),
  CONSTRAINT `chat_groups_created_by_foreign` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=4 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `chat_reads`;
CREATE TABLE `chat_reads` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `chat_key` varchar(32) NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `last_message_id` bigint(20) unsigned NOT NULL DEFAULT 0,
  `updated_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `chat_reads_chat_key_user_id_unique` (`chat_key`,`user_id`),
  KEY `chat_reads_user_id_index` (`user_id`),
  CONSTRAINT `chat_reads_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=15 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `code_sequences`;
CREATE TABLE `code_sequences` (
  `name` varchar(32) NOT NULL,
  `last_value` bigint(20) unsigned NOT NULL DEFAULT 0,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
INSERT INTO `code_sequences` VALUES
('bug',6),
('project',6),
('task',13);

DROP TABLE IF EXISTS `direct_conversations`;
CREATE TABLE `direct_conversations` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_low_id` bigint(20) unsigned NOT NULL,
  `user_high_id` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `direct_conversations_user_low_id_user_high_id_unique` (`user_low_id`,`user_high_id`),
  KEY `direct_conversations_user_high_id_index` (`user_high_id`),
  CONSTRAINT `direct_conversations_user_high_id_foreign` FOREIGN KEY (`user_high_id`) REFERENCES `users` (`id`),
  CONSTRAINT `direct_conversations_user_low_id_foreign` FOREIGN KEY (`user_low_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `failed_jobs`;
CREATE TABLE `failed_jobs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `uuid` varchar(255) NOT NULL,
  `connection` text NOT NULL,
  `queue` text NOT NULL,
  `payload` longtext NOT NULL,
  `exception` longtext NOT NULL,
  `failed_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `failed_jobs_uuid_unique` (`uuid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `github_items`;
CREATE TABLE `github_items` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `external_id` varchar(50) NOT NULL,
  `kind` enum('pr','commit') NOT NULL,
  `repo` varchar(140) NOT NULL,
  `ref` varchar(64) NOT NULL,
  `title` varchar(255) NOT NULL,
  `state` enum('open','merged','closed') DEFAULT NULL,
  `author` varchar(120) NOT NULL,
  `at` timestamp NOT NULL,
  `url` varchar(500) NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `github_items_repo_external_id_unique` (`repo`,`external_id`),
  KEY `github_items_repo_at_index` (`repo`,`at`),
  KEY `github_items_at_index` (`at`)
) ENGINE=InnoDB AUTO_INCREMENT=4 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `github_webhook_deliveries`;
CREATE TABLE `github_webhook_deliveries` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `delivery_id` varchar(120) NOT NULL,
  `event` varchar(64) NOT NULL,
  `received_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `github_webhook_deliveries_delivery_id_unique` (`delivery_id`)
) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `job_batches`;
CREATE TABLE `job_batches` (
  `id` varchar(255) NOT NULL,
  `name` varchar(255) NOT NULL,
  `total_jobs` int(11) NOT NULL,
  `pending_jobs` int(11) NOT NULL,
  `failed_jobs` int(11) NOT NULL,
  `failed_job_ids` longtext NOT NULL,
  `options` mediumtext DEFAULT NULL,
  `cancelled_at` int(11) DEFAULT NULL,
  `created_at` int(11) NOT NULL,
  `finished_at` int(11) DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `jobs`;
CREATE TABLE `jobs` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `queue` varchar(255) NOT NULL,
  `payload` longtext NOT NULL,
  `attempts` tinyint(3) unsigned NOT NULL,
  `reserved_at` int(10) unsigned DEFAULT NULL,
  `available_at` int(10) unsigned NOT NULL,
  `created_at` int(10) unsigned NOT NULL,
  PRIMARY KEY (`id`),
  KEY `jobs_queue_index` (`queue`)
) ENGINE=InnoDB AUTO_INCREMENT=27 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `login_security`;
CREATE TABLE `login_security` (
  `user_id` bigint(20) unsigned NOT NULL,
  `failed_logins` int(10) unsigned NOT NULL DEFAULT 0,
  `locked_until` timestamp NULL DEFAULT NULL,
  `last_login_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`user_id`),
  KEY `login_security_locked_until_index` (`locked_until`),
  CONSTRAINT `login_security_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `messages`;
CREATE TABLE `messages` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `chat_key` varchar(32) NOT NULL,
  `author_id` bigint(20) unsigned NOT NULL,
  `body` text NOT NULL,
  `reply_to_id` bigint(20) unsigned DEFAULT NULL,
  `mentions` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`mentions`)),
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `messages_author_id_foreign` (`author_id`),
  KEY `messages_reply_to_id_foreign` (`reply_to_id`),
  KEY `messages_chat_key_id_index` (`chat_key`,`id`),
  KEY `messages_chat_key_index` (`chat_key`),
  KEY `messages_created_at_index` (`created_at`),
  CONSTRAINT `messages_author_id_foreign` FOREIGN KEY (`author_id`) REFERENCES `users` (`id`),
  CONSTRAINT `messages_reply_to_id_foreign` FOREIGN KEY (`reply_to_id`) REFERENCES `messages` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=104 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `migrations`;
CREATE TABLE `migrations` (
  `id` int(10) unsigned NOT NULL AUTO_INCREMENT,
  `migration` varchar(255) NOT NULL,
  `batch` int(11) NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=16 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
INSERT INTO `migrations` VALUES
(1,'0001_01_01_000000_create_users_table',1),
(2,'0001_01_01_000001_create_cache_table',1),
(3,'0001_01_01_000002_create_jobs_table',1),
(4,'2026_11_21_064315_create_personal_access_tokens_table',1),
(5,'2026_11_21_070001_create_projects_tables',1),
(6,'2026_11_21_070002_create_tasks_tables',1),
(7,'2026_11_21_070003_create_notifications_and_attachments_tables',1),
(8,'2026_11_21_070004_create_announcements_and_bugs_tables',1),
(9,'2026_11_21_070005_create_chat_tables',1),
(10,'2026_11_21_070006_create_performance_and_audit_tables',1),
(11,'2026_11_21_070007_create_github_tables',1),
(12,'2026_11_21_080001_add_v2_features_tables',2),
(13,'2026_11_22_090001_add_nov22_features_tables',3),
(14,'2026_11_24_100001_add_nov24_features_tables',4),
(15,'2026_11_24_120001_align_trial_teams_schema',5);

DROP TABLE IF EXISTS `monitor_note_shares`;
CREATE TABLE `monitor_note_shares` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `note_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `label` varchar(255) NOT NULL,
  `label_en` varchar(255) DEFAULT NULL,
  `shared_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `seen_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `monitor_note_shares_note_id_user_id_unique` (`note_id`,`user_id`),
  KEY `monitor_note_shares_user_id_shared_at_index` (`user_id`,`shared_at`),
  CONSTRAINT `monitor_note_shares_note_id_foreign` FOREIGN KEY (`note_id`) REFERENCES `monitor_notes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `monitor_note_shares_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `monitor_notes`;
CREATE TABLE `monitor_notes` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `kind` enum('report','feedback','problem','request') NOT NULL,
  `title` varchar(160) NOT NULL,
  `body` text NOT NULL,
  `about_type` enum('team','person','project','general') NOT NULL,
  `about_id` bigint(20) unsigned DEFAULT NULL,
  `about_label` varchar(255) DEFAULT NULL,
  `about_label_en` varchar(255) DEFAULT NULL,
  `tone` enum('praise','improve') DEFAULT NULL,
  `subject` varchar(120) DEFAULT NULL,
  `people` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`people`)),
  `author_id` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `sent_at` timestamp NULL DEFAULT NULL,
  `ceo_state` enum('new','seen','done') DEFAULT NULL,
  `ceo_reply` text DEFAULT NULL,
  `replied_at` timestamp NULL DEFAULT NULL,
  `facts` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`facts`)),
  `triage` enum('pending','forwarded','answered') DEFAULT NULL,
  `forward_note` text DEFAULT NULL,
  `monitor_reply` text DEFAULT NULL,
  `triaged_by` bigint(20) unsigned DEFAULT NULL,
  `triaged_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `monitor_notes_author_id_created_at_index` (`author_id`,`created_at`),
  KEY `monitor_notes_kind_index` (`kind`),
  KEY `monitor_notes_sent_at_index` (`sent_at`),
  KEY `monitor_notes_triaged_by_foreign` (`triaged_by`),
  KEY `monitor_notes_triage_index` (`triage`),
  CONSTRAINT `monitor_notes_author_id_foreign` FOREIGN KEY (`author_id`) REFERENCES `users` (`id`),
  CONSTRAINT `monitor_notes_triaged_by_foreign` FOREIGN KEY (`triaged_by`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB AUTO_INCREMENT=9 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `notification_preferences`;
CREATE TABLE `notification_preferences` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint(20) unsigned NOT NULL,
  `kind` varchar(40) NOT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT 1,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `notification_preferences_user_id_kind_unique` (`user_id`,`kind`),
  CONSTRAINT `notification_preferences_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `notifications`;
CREATE TABLE `notifications` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint(20) unsigned NOT NULL,
  `kind` varchar(40) NOT NULL,
  `actor_id` bigint(20) unsigned DEFAULT NULL,
  `project_id` bigint(20) unsigned DEFAULT NULL,
  `task_id` bigint(20) unsigned DEFAULT NULL,
  `announcement_id` bigint(20) unsigned DEFAULT NULL,
  `bug_id` bigint(20) unsigned DEFAULT NULL,
  `chat_key` varchar(32) DEFAULT NULL,
  `team_id` bigint(20) unsigned DEFAULT NULL,
  `data` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`data`)),
  `dedup_key` varchar(191) DEFAULT NULL,
  `at` timestamp NOT NULL,
  `read` tinyint(1) NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  UNIQUE KEY `notifications_dedup_key_unique` (`dedup_key`),
  KEY `notifications_actor_id_foreign` (`actor_id`),
  KEY `notifications_user_id_read_at_index` (`user_id`,`read`,`at`),
  KEY `notifications_user_id_at_index` (`user_id`,`at`),
  KEY `notifications_kind_index` (`kind`),
  KEY `notifications_project_id_index` (`project_id`),
  KEY `notifications_task_id_index` (`task_id`),
  KEY `notifications_announcement_id_index` (`announcement_id`),
  KEY `notifications_bug_id_index` (`bug_id`),
  KEY `notifications_at_index` (`at`),
  KEY `notifications_read_index` (`read`),
  KEY `notifications_team_id_foreign` (`team_id`),
  CONSTRAINT `notifications_actor_id_foreign` FOREIGN KEY (`actor_id`) REFERENCES `users` (`id`),
  CONSTRAINT `notifications_team_id_foreign` FOREIGN KEY (`team_id`) REFERENCES `teams` (`id`) ON DELETE SET NULL,
  CONSTRAINT `notifications_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=31 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `password_reset_tokens`;
CREATE TABLE `password_reset_tokens` (
  `email` varchar(191) NOT NULL,
  `token` varchar(255) NOT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`email`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `performance_criteria`;
CREATE TABLE `performance_criteria` (
  `id` tinyint(3) unsigned NOT NULL,
  `items` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`items`)),
  `approved_by` bigint(20) unsigned DEFAULT NULL,
  `approved_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `performance_criteria_approved_by_foreign` (`approved_by`),
  CONSTRAINT `performance_criteria_approved_by_foreign` FOREIGN KEY (`approved_by`) REFERENCES `users` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
INSERT INTO `performance_criteria` VALUES
(1,'[{\"id\":\"completion\",\"name\":\"\\u0625\\u0646\\u062c\\u0627\\u0632 \\u0627\\u0644\\u0645\\u0647\\u0627\\u0645\",\"name_en\":\"Tasks done\",\"kind\":\"auto\",\"metric\":\"completion\",\"weight\":25},{\"id\":\"on_time\",\"name\":\"\\u0627\\u0644\\u0627\\u0644\\u062a\\u0632\\u0627\\u0645 \\u0628\\u0627\\u0644\\u0645\\u0648\\u0627\\u0639\\u064a\\u062f\",\"name_en\":\"On time\",\"kind\":\"auto\",\"metric\":\"on_time\",\"weight\":25},{\"id\":\"quality\",\"name\":\"\\u062c\\u0648\\u062f\\u0629 \\u0627\\u0644\\u0639\\u0645\\u0644\",\"name_en\":\"Quality of work\",\"kind\":\"manual\",\"weight\":30},{\"id\":\"teamwork\",\"name\":\"\\u0627\\u0644\\u062a\\u0639\\u0627\\u0648\\u0646 \\u0648\\u0627\\u0644\\u062a\\u0648\\u0627\\u0635\\u0644\",\"name_en\":\"Teamwork\",\"kind\":\"manual\",\"weight\":20}]',1,'2026-11-21 07:19:51','2026-11-21 07:19:51');

DROP TABLE IF EXISTS `performance_review_scores`;
CREATE TABLE `performance_review_scores` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `review_id` bigint(20) unsigned NOT NULL,
  `criterion_id` varchar(64) NOT NULL,
  `criterion_name` varchar(120) NOT NULL,
  `criterion_name_en` varchar(120) NOT NULL,
  `kind` enum('auto','manual') NOT NULL,
  `metric` varchar(32) DEFAULT NULL,
  `weight` tinyint(3) unsigned NOT NULL,
  `stars` tinyint(3) unsigned DEFAULT NULL,
  `score` tinyint(3) unsigned NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `performance_review_scores_review_id_criterion_id_unique` (`review_id`,`criterion_id`),
  CONSTRAINT `performance_review_scores_review_id_foreign` FOREIGN KEY (`review_id`) REFERENCES `performance_reviews` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=9 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `performance_reviews`;
CREATE TABLE `performance_reviews` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint(20) unsigned NOT NULL,
  `reviewer_id` bigint(20) unsigned NOT NULL,
  `period_from` date NOT NULL,
  `period_to` date NOT NULL,
  `scores` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`scores`)),
  `criteria` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`criteria`)),
  `total` tinyint(3) unsigned NOT NULL,
  `note` text NOT NULL,
  `perf` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`perf`)),
  `state` enum('draft','final') NOT NULL DEFAULT 'draft',
  `finalized_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `performance_reviews_user_id_period_to_index` (`user_id`,`period_to`),
  KEY `performance_reviews_reviewer_id_index` (`reviewer_id`),
  KEY `performance_reviews_state_index` (`state`),
  CONSTRAINT `performance_reviews_reviewer_id_foreign` FOREIGN KEY (`reviewer_id`) REFERENCES `users` (`id`),
  CONSTRAINT `performance_reviews_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `personal_access_tokens`;
CREATE TABLE `personal_access_tokens` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `tokenable_type` varchar(255) NOT NULL,
  `tokenable_id` bigint(20) unsigned NOT NULL,
  `name` text NOT NULL,
  `token` varchar(64) NOT NULL,
  `abilities` text DEFAULT NULL,
  `session_id` varchar(64) DEFAULT NULL,
  `last_used_at` timestamp NULL DEFAULT NULL,
  `expires_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `personal_access_tokens_token_unique` (`token`),
  KEY `personal_access_tokens_tokenable_type_tokenable_id_index` (`tokenable_type`,`tokenable_id`),
  KEY `personal_access_tokens_expires_at_index` (`expires_at`),
  KEY `personal_access_tokens_session_id_index` (`session_id`)
) ENGINE=InnoDB AUTO_INCREMENT=31 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `project_events`;
CREATE TABLE `project_events` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint(20) unsigned NOT NULL,
  `actor_id` bigint(20) unsigned NOT NULL,
  `kind` varchar(40) NOT NULL,
  `from` varchar(255) DEFAULT NULL,
  `to` varchar(255) DEFAULT NULL,
  `note` varchar(300) DEFAULT NULL,
  `at` timestamp NOT NULL,
  PRIMARY KEY (`id`),
  KEY `project_events_actor_id_foreign` (`actor_id`),
  KEY `project_events_project_id_at_index` (`project_id`,`at`),
  KEY `project_events_at_index` (`at`),
  CONSTRAINT `project_events_actor_id_foreign` FOREIGN KEY (`actor_id`) REFERENCES `users` (`id`),
  CONSTRAINT `project_events_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=15 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `project_github_settings`;
CREATE TABLE `project_github_settings` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint(20) unsigned NOT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT 0,
  `repos` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`repos`)),
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `project_github_settings_project_id_unique` (`project_id`),
  CONSTRAINT `project_github_settings_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `project_members`;
CREATE TABLE `project_members` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint(20) unsigned NOT NULL,
  `user_id` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `project_members_project_id_user_id_unique` (`project_id`,`user_id`),
  KEY `project_members_user_id_index` (`user_id`),
  CONSTRAINT `project_members_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`),
  CONSTRAINT `project_members_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=44 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `project_phases`;
CREATE TABLE `project_phases` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint(20) unsigned NOT NULL,
  `name` varchar(80) NOT NULL,
  `position` smallint(5) unsigned NOT NULL DEFAULT 0,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `project_phases_project_id_name_unique` (`project_id`,`name`),
  KEY `project_phases_project_id_position_index` (`project_id`,`position`),
  CONSTRAINT `project_phases_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=21 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `project_statuses`;
CREATE TABLE `project_statuses` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint(20) unsigned NOT NULL,
  `key` varchar(64) NOT NULL,
  `name` varchar(40) NOT NULL,
  `name_en` varchar(40) NOT NULL,
  `tone` enum('new','todo','progress','review','testing','done','blocked') NOT NULL,
  `position` smallint(5) unsigned NOT NULL DEFAULT 0,
  `is_base` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `project_statuses_project_id_key_unique` (`project_id`,`key`),
  UNIQUE KEY `project_statuses_project_id_name_unique` (`project_id`,`name`),
  KEY `project_statuses_project_id_position_index` (`project_id`,`position`),
  CONSTRAINT `project_statuses_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=78 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `project_templates`;
CREATE TABLE `project_templates` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(80) NOT NULL,
  `description` text NOT NULL,
  `statuses` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`statuses`)),
  `phases` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`phases`)),
  `uses_sprints` tinyint(1) NOT NULL DEFAULT 0,
  `tasks` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL CHECK (json_valid(`tasks`)),
  `from_project` varchar(120) DEFAULT NULL,
  `created_by` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `project_templates_name_unique` (`name`),
  KEY `project_templates_created_by_foreign` (`created_by`),
  CONSTRAINT `project_templates_created_by_foreign` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `projects`;
CREATE TABLE `projects` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `code` varchar(32) NOT NULL,
  `name` varchar(120) NOT NULL,
  `name_en` varchar(120) DEFAULT NULL,
  `description` text NOT NULL,
  `pm_id` bigint(20) unsigned NOT NULL,
  `start` date NOT NULL,
  `end` date NOT NULL,
  `state` enum('planned','active','paused','done') NOT NULL DEFAULT 'active',
  `priority` enum('high','medium','low') NOT NULL DEFAULT 'medium',
  `uses_sprints` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `projects_code_unique` (`code`),
  KEY `projects_pm_id_foreign` (`pm_id`),
  KEY `projects_start_index` (`start`),
  KEY `projects_end_index` (`end`),
  KEY `projects_state_index` (`state`),
  CONSTRAINT `projects_pm_id_foreign` FOREIGN KEY (`pm_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=12 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `push_subscriptions`;
CREATE TABLE `push_subscriptions` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint(20) unsigned NOT NULL,
  `session_id` varchar(64) DEFAULT NULL,
  `endpoint_hash` char(64) NOT NULL,
  `endpoint` text NOT NULL,
  `p256dh` varchar(255) NOT NULL,
  `auth` varchar(255) NOT NULL,
  `device` varchar(120) NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `last_used_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `push_subscriptions_endpoint_hash_unique` (`endpoint_hash`),
  KEY `push_subscriptions_user_id_index` (`user_id`),
  KEY `push_subscriptions_session_id_index` (`session_id`),
  KEY `push_subscriptions_last_used_at_index` (`last_used_at`),
  CONSTRAINT `push_subscriptions_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=6 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `ratings`;
CREATE TABLE `ratings` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `subject_type` enum('team','person') NOT NULL,
  `subject_id` bigint(20) unsigned NOT NULL,
  `rater_id` bigint(20) unsigned NOT NULL,
  `score` tinyint(3) unsigned NOT NULL,
  `reason_type` varchar(32) NOT NULL,
  `reason` text NOT NULL,
  `project_id` bigint(20) unsigned DEFAULT NULL,
  `project_name` varchar(160) DEFAULT NULL,
  `project_name_en` varchar(160) DEFAULT NULL,
  `period_from` date DEFAULT NULL,
  `period_to` date DEFAULT NULL,
  `private` tinyint(1) NOT NULL DEFAULT 0,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `edited_at` timestamp NULL DEFAULT NULL,
  `edited_by` bigint(20) unsigned DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `ratings_project_id_foreign` (`project_id`),
  KEY `ratings_edited_by_foreign` (`edited_by`),
  KEY `ratings_subject_type_subject_id_created_at_index` (`subject_type`,`subject_id`,`created_at`),
  KEY `ratings_rater_id_created_at_index` (`rater_id`,`created_at`),
  CONSTRAINT `ratings_edited_by_foreign` FOREIGN KEY (`edited_by`) REFERENCES `users` (`id`) ON DELETE SET NULL,
  CONSTRAINT `ratings_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`) ON DELETE SET NULL,
  CONSTRAINT `ratings_rater_id_foreign` FOREIGN KEY (`rater_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=7 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `sessions`;
CREATE TABLE `sessions` (
  `id` varchar(191) NOT NULL,
  `user_id` bigint(20) unsigned DEFAULT NULL,
  `ip_address` varchar(45) DEFAULT NULL,
  `user_agent` text DEFAULT NULL,
  `payload` longtext NOT NULL,
  `last_activity` int(11) NOT NULL,
  PRIMARY KEY (`id`),
  KEY `sessions_user_id_index` (`user_id`),
  KEY `sessions_last_activity_index` (`last_activity`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `sprints`;
CREATE TABLE `sprints` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint(20) unsigned NOT NULL,
  `name` varchar(60) NOT NULL,
  `start` date NOT NULL,
  `end` date NOT NULL,
  `closed_at` timestamp NULL DEFAULT NULL,
  `result` longtext CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL CHECK (json_valid(`result`)),
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `sprints_project_id_start_end_index` (`project_id`,`start`,`end`),
  CONSTRAINT `sprints_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=9 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `task_approvals`;
CREATE TABLE `task_approvals` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `task_id` bigint(20) unsigned NOT NULL,
  `state` enum('pending','approved','rejected','cancelled') NOT NULL DEFAULT 'pending',
  `from_status` varchar(64) NOT NULL,
  `to_status` varchar(64) NOT NULL,
  `requested_by` bigint(20) unsigned NOT NULL,
  `requested_at` timestamp NOT NULL,
  `decided_by` bigint(20) unsigned DEFAULT NULL,
  `decided_at` timestamp NULL DEFAULT NULL,
  `note` varchar(300) DEFAULT NULL,
  `is_current` tinyint(1) NOT NULL DEFAULT 1,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `task_approvals_requested_by_foreign` (`requested_by`),
  KEY `task_approvals_decided_by_foreign` (`decided_by`),
  KEY `task_approvals_task_id_is_current_index` (`task_id`,`is_current`),
  CONSTRAINT `task_approvals_decided_by_foreign` FOREIGN KEY (`decided_by`) REFERENCES `users` (`id`),
  CONSTRAINT `task_approvals_requested_by_foreign` FOREIGN KEY (`requested_by`) REFERENCES `users` (`id`),
  CONSTRAINT `task_approvals_task_id_foreign` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=9 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `task_comments`;
CREATE TABLE `task_comments` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `task_id` bigint(20) unsigned NOT NULL,
  `author_id` bigint(20) unsigned NOT NULL,
  `body` text NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `task_comments_author_id_foreign` (`author_id`),
  KEY `task_comments_task_id_created_at_index` (`task_id`,`created_at`),
  CONSTRAINT `task_comments_author_id_foreign` FOREIGN KEY (`author_id`) REFERENCES `users` (`id`),
  CONSTRAINT `task_comments_task_id_foreign` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=6 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `task_dependencies`;
CREATE TABLE `task_dependencies` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `task_id` bigint(20) unsigned NOT NULL,
  `depends_on_task_id` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  UNIQUE KEY `task_dependencies_task_id_depends_on_task_id_unique` (`task_id`,`depends_on_task_id`),
  KEY `task_dependencies_depends_on_task_id_index` (`depends_on_task_id`),
  CONSTRAINT `task_dependencies_depends_on_task_id_foreign` FOREIGN KEY (`depends_on_task_id`) REFERENCES `tasks` (`id`),
  CONSTRAINT `task_dependencies_task_id_foreign` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `task_events`;
CREATE TABLE `task_events` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `task_id` bigint(20) unsigned NOT NULL,
  `actor_id` bigint(20) unsigned NOT NULL,
  `kind` varchar(40) NOT NULL,
  `from` varchar(255) DEFAULT NULL,
  `to` varchar(255) DEFAULT NULL,
  `note` varchar(300) DEFAULT NULL,
  `at` timestamp NOT NULL,
  PRIMARY KEY (`id`),
  KEY `task_events_actor_id_foreign` (`actor_id`),
  KEY `task_events_task_id_at_index` (`task_id`,`at`),
  KEY `task_events_at_index` (`at`),
  CONSTRAINT `task_events_actor_id_foreign` FOREIGN KEY (`actor_id`) REFERENCES `users` (`id`),
  CONSTRAINT `task_events_task_id_foreign` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=34 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `tasks`;
CREATE TABLE `tasks` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `code` varchar(32) NOT NULL,
  `project_id` bigint(20) unsigned NOT NULL,
  `phase_id` bigint(20) unsigned DEFAULT NULL,
  `sprint_id` bigint(20) unsigned DEFAULT NULL,
  `title` varchar(160) NOT NULL,
  `description` text NOT NULL,
  `assignee_id` bigint(20) unsigned DEFAULT NULL,
  `start` date DEFAULT NULL,
  `due` date DEFAULT NULL,
  `priority` enum('high','medium','low') NOT NULL DEFAULT 'medium',
  `status` varchar(64) NOT NULL,
  `blocked_reason` varchar(300) DEFAULT NULL,
  `subtasks_done` int(10) unsigned NOT NULL DEFAULT 0,
  `subtasks_total` int(10) unsigned NOT NULL DEFAULT 0,
  `created_by` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `tasks_code_unique` (`code`),
  KEY `tasks_phase_id_foreign` (`phase_id`),
  KEY `tasks_sprint_id_foreign` (`sprint_id`),
  KEY `tasks_created_by_foreign` (`created_by`),
  KEY `tasks_project_id_status_index` (`project_id`,`status`),
  KEY `tasks_assignee_id_due_index` (`assignee_id`,`due`),
  KEY `tasks_due_index` (`due`),
  KEY `tasks_status_index` (`status`),
  CONSTRAINT `tasks_assignee_id_foreign` FOREIGN KEY (`assignee_id`) REFERENCES `users` (`id`),
  CONSTRAINT `tasks_created_by_foreign` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`),
  CONSTRAINT `tasks_phase_id_foreign` FOREIGN KEY (`phase_id`) REFERENCES `project_phases` (`id`) ON DELETE SET NULL,
  CONSTRAINT `tasks_project_id_foreign` FOREIGN KEY (`project_id`) REFERENCES `projects` (`id`),
  CONSTRAINT `tasks_sprint_id_foreign` FOREIGN KEY (`sprint_id`) REFERENCES `sprints` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB AUTO_INCREMENT=26 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `teams`;
CREATE TABLE `teams` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(80) NOT NULL,
  `name_en` varchar(80) DEFAULT NULL,
  `lead_id` bigint(20) unsigned DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `teams_name_unique` (`name`),
  KEY `teams_lead_id_index` (`lead_id`),
  CONSTRAINT `teams_lead_id_foreign` FOREIGN KEY (`lead_id`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB AUTO_INCREMENT=8 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `trial_entries`;
CREATE TABLE `trial_entries` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `member_id` bigint(20) unsigned NOT NULL,
  `score` tinyint(3) unsigned DEFAULT NULL,
  `text` varchar(1000) DEFAULT NULL,
  `by_id` bigint(20) unsigned NOT NULL,
  `at` timestamp NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`id`),
  KEY `trial_entries_by_id_foreign` (`by_id`),
  KEY `trial_entries_member_id_at_index` (`member_id`,`at`),
  CONSTRAINT `trial_entries_by_id_foreign` FOREIGN KEY (`by_id`) REFERENCES `users` (`id`) ON DELETE CASCADE,
  CONSTRAINT `trial_entries_member_id_foreign` FOREIGN KEY (`member_id`) REFERENCES `trial_members` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=7 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `trial_members`;
CREATE TABLE `trial_members` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `team_id` bigint(20) unsigned NOT NULL,
  `name` varchar(120) NOT NULL,
  `specialty` varchar(80) NOT NULL,
  `phone` varchar(30) DEFAULT NULL,
  `email` varchar(255) DEFAULT NULL,
  `joined_on` date NOT NULL,
  `decision` enum('pending','accepted','rejected') NOT NULL DEFAULT 'pending',
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `trial_members_team_id_index` (`team_id`),
  CONSTRAINT `trial_members_team_id_foreign` FOREIGN KEY (`team_id`) REFERENCES `trial_teams` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `trial_teams`;
CREATE TABLE `trial_teams` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(80) NOT NULL,
  `goal` varchar(500) DEFAULT NULL,
  `started_on` date NOT NULL,
  `status` enum('running','ended') NOT NULL DEFAULT 'running',
  `ended_on` date DEFAULT NULL,
  `created_by` bigint(20) unsigned NOT NULL,
  `created_at` timestamp NOT NULL DEFAULT current_timestamp(),
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `trial_teams_created_by_status_index` (`created_by`,`status`),
  KEY `trial_teams_status_index` (`status`),
  CONSTRAINT `trial_teams_created_by_foreign` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

DROP TABLE IF EXISTS `users`;
CREATE TABLE `users` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(120) NOT NULL,
  `name_en` varchar(120) DEFAULT NULL,
  `email` varchar(191) NOT NULL,
  `phone` varchar(40) DEFAULT NULL,
  `role` enum('ceo','pm','monitor','lead','employee','admin') NOT NULL,
  `custom_role` varchar(40) DEFAULT NULL,
  `birth_date` date DEFAULT NULL,
  `preferred_locale` varchar(8) NOT NULL DEFAULT 'ar',
  `title` varchar(80) NOT NULL DEFAULT '',
  `team_id` bigint(20) unsigned DEFAULT NULL,
  `active` tinyint(1) NOT NULL DEFAULT 1,
  `password` varchar(255) NOT NULL,
  `remember_token` varchar(100) DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `users_email_unique` (`email`),
  KEY `users_team_id_foreign` (`team_id`),
  KEY `users_phone_index` (`phone`),
  KEY `users_role_index` (`role`),
  KEY `users_active_index` (`active`),
  KEY `users_custom_role_index` (`custom_role`),
  KEY `users_birth_date_index` (`birth_date`),
  CONSTRAINT `users_team_id_foreign` FOREIGN KEY (`team_id`) REFERENCES `teams` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB AUTO_INCREMENT=26 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
